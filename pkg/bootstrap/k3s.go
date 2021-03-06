package bootstrap

import (
	"bytes"
	"fmt"
	"github.com/avast/retry-go"
	"github.com/hashicorp/go-multierror"
	"github.com/innobead/kubefire/internal/config"
	"github.com/innobead/kubefire/pkg/constants"
	"github.com/innobead/kubefire/pkg/data"
	"github.com/innobead/kubefire/pkg/node"
	"github.com/innobead/kubefire/pkg/script"
	utilssh "github.com/innobead/kubefire/pkg/util/ssh"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"strings"
	"sync"
	"time"
)

type K3sExtraOptions struct {
	ServerInstallOpts string
	AgentInstallOpts  string
	ExtraOptions      string
}

type K3sBootstrapper struct {
	nodeManager node.Manager
}

func NewK3sBootstrapper() *K3sBootstrapper {
	return &K3sBootstrapper{}
}

func (k *K3sBootstrapper) SetNodeManager(nodeManager node.Manager) {
	k.nodeManager = nodeManager
}

func (k *K3sBootstrapper) Deploy(cluster *data.Cluster, before func() error) error {
	if before != nil {
		if err := before(); err != nil {
			return err
		}
	}

	extraOptions := cluster.Spec.ParseExtraOptions(&K3sExtraOptions{
		ExtraOptions: config.K3sVersionsEnvVars(cluster.Spec.Version).String(),
	}).(K3sExtraOptions)

	if err := k.nodeManager.WaitNodesRunning(cluster.Name, 5); err != nil {
		return errors.WithMessage(err, "some nodes are not running")
	}

	if err := k.init(cluster); err != nil {
		return err
	}

	firstMaster, err := k.nodeManager.GetNode(node.Name(cluster.Name, node.Master, 1))
	if err != nil {
		return err
	}

	firstMaster.Spec.Cluster = &cluster.Spec

	joinToken, err := k.bootstrap(firstMaster, len(cluster.Nodes) == 1, extraOptions)
	if err != nil {
		return err
	}

	nodes, err := k.nodeManager.ListNodes(cluster.Name)
	if err != nil {
		return err
	}

	if len(nodes) == 0 {
		return errors.New("no nodes available")
	}

	for _, n := range nodes {
		if n.Name == firstMaster.Name {
			continue
		}
		n.Spec.Cluster = &cluster.Spec

		if err := k.join(n, firstMaster.Status.IPAddresses, joinToken, extraOptions); err != nil {
			return err
		}
	}

	return nil
}

func (k *K3sBootstrapper) DownloadKubeConfig(cluster *data.Cluster, destDir string) (string, error) {
	return downloadKubeConfig(k.nodeManager, cluster, "/etc/rancher/k3s/k3s.yaml", destDir)
}

func (k *K3sBootstrapper) Prepare(cluster *data.Cluster, force bool) error {
	return nil
}

func (k *K3sBootstrapper) Type() string {
	return constants.K3S
}

func (k *K3sBootstrapper) init(cluster *data.Cluster) error {
	logrus.WithField("cluster", cluster.Name).Infoln("initializing cluster")

	wgInitNodes := sync.WaitGroup{}
	wgInitNodes.Add(len(cluster.Nodes))

	chErr := make(chan error, len(cluster.Nodes))

	for _, n := range cluster.Nodes {
		logrus.WithField("node", n.Name).Infoln("initializing node")

		go func(n *data.Node) {
			defer wgInitNodes.Done()

			_ = retry.Do(func() error {
				sshClient, err := utilssh.NewClient(
					n.Name,
					cluster.Spec.Prikey,
					"root",
					n.Status.IPAddresses,
					nil,
				)
				if err != nil {
					return err
				}
				defer sshClient.Close()

				cmds := []string{
					"swapoff -a",
					fmt.Sprintf("curl -sSLO %s", script.RemoteScriptUrl(script.InstallPrerequisitesK3s)),
					fmt.Sprintf("chmod +x %s", script.InstallPrerequisitesK3s),
					fmt.Sprintf("%s ./%s", config.K3sVersionsEnvVars(cluster.Spec.Version).String(), script.InstallPrerequisitesK3s),
				}

				err = sshClient.Run(nil, nil, cmds...)
				if err != nil {
					chErr <- errors.WithMessagef(err, "failed on node (%s)", n.Name)
				}

				return nil
			},
				retry.Delay(10*time.Second),
				retry.MaxDelay(1*time.Minute),
			)
		}(n)
	}

	logrus.Info("waiting all nodes initialization finished")

	wgInitNodes.Wait()
	close(chErr)

	var err error
	for {
		e, ok := <-chErr
		if !ok {
			break
		}

		if e != nil {
			err = multierror.Append(err, e)
		}
	}

	return err
}

func (k *K3sBootstrapper) bootstrap(node *data.Node, isSingleNode bool, extraOptions K3sExtraOptions) (token string, err error) {
	logrus.WithField("node", node.Name).Infoln("bootstrapping the first master node")

	sshClient, err := utilssh.NewClient(
		node.Name,
		node.Spec.Cluster.Prikey,
		"root",
		node.Status.IPAddresses,
		nil,
	)
	if err != nil {
		return "", err
	}
	defer sshClient.Close()

	k3sOpts := []string{
		fmt.Sprintf("--bind-address=%s", node.Status.IPAddresses),
	}
	tokenBuf := bytes.Buffer{}

	if !isSingleNode {
		k3sOpts = append(k3sOpts, "--cluster-init")
	}

	if extraOptions.ServerInstallOpts != "" {
		k3sOpts = append(k3sOpts, extraOptions.ServerInstallOpts)
	}

	cmds := []struct {
		cmdline string
		before  utilssh.Callback
	}{
		{
			cmdline: fmt.Sprintf(`INSTALL_K3S_EXEC="%s" %s k3s-install.sh `, strings.Join(k3sOpts, " "), extraOptions.ExtraOptions),
		},
		{
			cmdline: "cat /var/lib/rancher/k3s/server/node-token",
			before: func(session *ssh.Session) bool {
				session.Stdout = &tokenBuf
				return true
			},
		},
	}

	for _, c := range cmds {
		err := sshClient.Run(c.before, nil, c.cmdline)
		if err != nil {
			return "", errors.WithStack(err)
		}
	}

	return strings.TrimSuffix(tokenBuf.String(), "\n"), nil
}

func (k *K3sBootstrapper) join(node *data.Node, apiServerAddress string, joinToken string, extraOptions K3sExtraOptions) error {
	logrus.WithField("node", node.Name).Infoln("joining node")

	sshClient, err := utilssh.NewClient(
		node.Name,
		node.Spec.Cluster.Prikey,
		"root",
		node.Status.IPAddresses,
		nil,
	)
	if err != nil {
		return err
	}
	defer sshClient.Close()

	var k3sOpts []string
	cmd := fmt.Sprintf("K3S_URL=https://%s:6443 K3S_TOKEN=%s k3s-install.sh", apiServerAddress, joinToken)

	if node.IsMaster() {
		k3sOpts = append(k3sOpts, "--server")

		if extraOptions.ServerInstallOpts != "" {
			k3sOpts = append(k3sOpts, extraOptions.ServerInstallOpts)
		}
	} else {
		if extraOptions.AgentInstallOpts != "" {
			k3sOpts = append(k3sOpts, extraOptions.AgentInstallOpts)
		}
	}

	cmd = fmt.Sprintf(`INSTALL_K3S_EXEC="%s" %s`, strings.Join(k3sOpts, " "), extraOptions.ExtraOptions) + cmd

	if err := sshClient.Run(nil, nil, cmd); err != nil {
		return errors.WithStack(err)
	}

	return nil
}
