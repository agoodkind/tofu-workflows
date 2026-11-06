package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/agoodkind/tofu-workflows/internal/config"
	"github.com/agoodkind/tofu-workflows/internal/credentials"
	"github.com/agoodkind/tofu-workflows/internal/input"
)

func credentialCommand(args []string, stderr io.Writer) error {
	var checkout, configuration, environmentName string
	var cleanup bool
	flags := flag.NewFlagSet("credentials", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&checkout, "checkout", "", "Existing checkout containing protected configuration.")
	flags.StringVar(&configuration, "config", "", "Deployment set JSON path relative to the checkout.")
	flags.StringVar(&environmentName, "environment", "", "Exact environment name in the protected deployment set.")
	flags.BoolVar(&cleanup, "cleanup", false, "Remove credential files owned by this environment's installation.")
	if err := flags.Parse(args); err != nil {
		slog.Error("Credential command arguments are invalid", "err", err)
		return fmt.Errorf("parse credentials arguments: %w", err)
	}
	if flags.NArg() != 0 {
		return errors.New("credentials does not accept positional arguments")
	}
	if checkout == "" || configuration == "" || environmentName == "" {
		return errors.New("--checkout, --config, and --environment are required")
	}
	path, err := input.RegularFile(checkout, configuration)
	if err != nil {
		return fmt.Errorf("resolve configuration: %w", err)
	}
	set, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	for _, environment := range []config.Environment{set.Validation, set.Production} {
		if environment.Name != environmentName {
			continue
		}
		if cleanup {
			if err := credentials.Cleanup(environment.SecretFiles); err != nil {
				return fmt.Errorf("clean up credential files: %w", err)
			}
			return nil
		}
		if err := credentials.InstallEnvironment(environment, os.Getenv("GITHUB_ENV")); err != nil {
			return fmt.Errorf("install credential bundles: %w", err)
		}
		return nil
	}
	return errors.New("--environment must match an environment in the protected deployment set")
}
