package cluster

import (
	"fmt"
	"github.com/innobead/kubefire/internal/di"
	"github.com/innobead/kubefire/internal/validate"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

var envCmd = &cobra.Command{
	Use:   "env [name]",
	Short: "Print environment values of cluster",
	Args:  validate.OneArg("cluster name"),
	PreRunE: func(cmd *cobra.Command, args []string) error {
		return validate.CheckClusterExist(args[0])
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		cluster, err := di.ConfigManager().GetCluster(name)
		if err != nil {
			return errors.WithMessagef(err, "failed to get cluster (%s) config", name)
		}

		fmt.Printf("KUBECONFIG=%s\n", cluster.LocalKubeConfig())

		return nil
	},
}
