package validate

import (
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

func OneArg(name string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := cobra.ExactArgs(1)(cmd, args); err != nil {
			return errors.WithMessagef(err, "missing %s", name)
		}

		return nil
	}
}

func MinimumArgs(name string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := cobra.MinimumNArgs(1)(cmd, args); err != nil {
			return errors.WithMessagef(err, "missing %s", name)
		}

		return nil
	}
}
