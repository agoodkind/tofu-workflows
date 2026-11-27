// Package command defines the Terrakube deployment queue's command line.
package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/agoodkind/tofu-workflows/internal/terrakubequeue"
	"github.com/spf13/cobra"
)

const (
	rootUsage               = "terrakube-queue <deployment> <checkout>"
	matrixCommandName       = "matrix"
	matrixUsage             = matrixCommandName + " <checkout> <" + matrixDeployMode + "|" + matrixPreviewMode + ">"
	affectedCommandName     = "affected"
	affectedUsage           = affectedCommandName + " <checkout> <root> <base> <head>"
	reservedHelpCommandName = "_help"
	matrixDeployMode        = "deploy"
	matrixPreviewMode       = "preview"
	deployArgCount          = 2
	matrixArgCount          = 2
	affectedArgCount        = 4
)

var errMatrixMode = errors.New(
	"matrix mode must be " + matrixDeployMode + " or " + matrixPreviewMode,
)

// Operations defines the queue operations available through the command line.
type Operations interface {
	Deploy(ctx context.Context, deployment, checkout string) error
	Matrix(
		ctx context.Context,
		checkout string,
		includeDisabled bool,
	) ([]terrakubequeue.Target, error)
	Affected(ctx context.Context, checkout, root, base, head string) (bool, error)
}

// New constructs the Terrakube deployment queue command.
//
// Deploy one target with the root command. Matrix and affected print
// results for workflows.
//
// For example, run the command with an injected service.
//
//	err := command.New(service).ExecuteContext(ctx)
func New(operations Operations) *cobra.Command {
	root := &cobra.Command{
		Use:   rootUsage,
		Short: "Submit reviewed commits to Terrakube in merge order",
		Args:  cobra.ExactArgs(deployArgCount),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := operations.Deploy(cmd.Context(), args[0], args[1]); err != nil {
				slog.ErrorContext(
					cmd.Context(),
					"deploy workspace",
					"workspace",
					args[0],
					"err",
					err,
				)

				return fmt.Errorf("deploy %s: %w", args[0], err)
			}

			return nil
		},
		SilenceErrors:         true,
		SilenceUsage:          true,
		DisableFlagsInUseLine: true,
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetHelpCommand(&cobra.Command{
		Use:    reservedHelpCommandName,
		Hidden: true,
	})
	root.AddCommand(matrixCommand(operations), affectedCommand(operations))

	return root
}

func matrixCommand(operations Operations) *cobra.Command {
	return &cobra.Command{
		Use:                   matrixUsage,
		Short:                 "Print the deployment targets as JSON",
		Args:                  validateMatrixArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			targets, err := operations.Matrix(cmd.Context(), args[0], args[1] == matrixPreviewMode)
			if err != nil {
				return fmt.Errorf("read %s deployment matrix: %w", args[1], err)
			}

			if targets == nil {
				targets = []terrakubequeue.Target{}
			}

			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(targets); err != nil {
				slog.ErrorContext(cmd.Context(), "write target matrix", "err", err)

				return fmt.Errorf("write target matrix: %w", err)
			}

			return nil
		},
	}
}

func validateMatrixArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.ExactArgs(matrixArgCount)(cmd, args); err != nil {
		return err
	}

	if args[1] != matrixDeployMode && args[1] != matrixPreviewMode {
		return errMatrixMode
	}

	return nil
}

func affectedCommand(operations Operations) *cobra.Command {
	return &cobra.Command{
		Use:                   affectedUsage,
		Short:                 "Check whether a commit changes a deployment target",
		Args:                  cobra.ExactArgs(affectedArgCount),
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			changed, err := operations.Affected(cmd.Context(), args[0], args[1], args[2], args[3])
			if err != nil {
				return fmt.Errorf("check changes to %s: %w", args[1], err)
			}

			if _, err := fmt.Fprintln(cmd.OutOrStdout(), changed); err != nil {
				slog.ErrorContext(cmd.Context(), "write affected result", "err", err)

				return fmt.Errorf("write affected result: %w", err)
			}

			return nil
		},
	}
}
