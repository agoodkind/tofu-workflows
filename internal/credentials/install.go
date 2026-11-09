// Package credentials installs protected secret bundles for one Actions job.
package credentials

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/agoodkind/tofu-workflows/internal/config"
	"github.com/agoodkind/tofu-workflows/internal/input"
	githubactions "github.com/sethvargo/go-githubactions"
)

// Install validates one bundle before installing its files and environment
// values.
func Install(raw string, allowed []string, files map[string]string, environmentFile string) error {
	values, err := decode(raw, allowed)
	if err != nil {
		return err
	}

	return install(values, files, environmentFile)
}

// InstallEnvironment reads and validates both bundles before writing
// credentials.
func InstallEnvironment(environment config.Environment, environmentFile string) error {
	backend, err := decode(os.Getenv("TF_BACKEND_ENV"), environment.BackendSecretEnvNames)
	if err != nil {
		slog.Error("Backend credential bundle validation failed", "err", err)
		return fmt.Errorf("TF_BACKEND_ENV: %w", err)
	}

	provider, err := decode(os.Getenv("TF_PROVIDER_ENV"), environment.SecretEnvNames)
	if err != nil {
		return fmt.Errorf("TF_PROVIDER_ENV: %w", err)
	}

	for name, value := range provider {
		if _, exists := backend[name]; exists {
			return errors.New("secret allowlists must not overlap")
		}

		backend[name] = value
	}

	data, err := json.Marshal(backend)
	if err != nil {
		return fmt.Errorf("encode validated bundles: %w", err)
	}

	allowed := append(
		slices.Clone(environment.BackendSecretEnvNames),
		environment.SecretEnvNames...,
	)

	return Install(string(data), allowed, environment.SecretFiles, environmentFile)
}

func decode(raw string, allowed []string) (map[string]string, error) {
	values, err := input.UnmarshalStringMap([]byte(raw))
	if err != nil || values == nil {
		return nil, errors.New(
			"bundle must contain a JSON object with unique keys and string values",
		)
	}

	names := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		if !config.SecretEnvironmentName(name) || names[name] {
			return nil, errors.New("secret allowlist contains a forbidden or duplicate name")
		}

		names[name] = true
	}

	for name, value := range values {
		if !names[name] || !config.SecretEnvironmentName(name) {
			return nil, errors.New("bundle contains an undeclared or forbidden secret name")
		}

		if value == "" || strings.ContainsRune(value, '\x00') {
			return nil, errors.New("bundle contains an empty value or a NUL byte")
		}
	}

	if len(values) != len(names) {
		return nil, errors.New("bundle is missing a required secret")
	}

	return values, nil
}

func install(values, files map[string]string, environmentFile string) error {
	for name := range files {
		if _, exists := values[name]; !exists {
			return errors.New("secret file must reference a declared secret")
		}
	}

	storage, err := prepareFiles(files)
	if err != nil {
		slog.Error("Credential file validation failed", "err", err)
		return err
	}
	defer storage.close()
	environment, err := openEnvironment(environmentFile)
	if err != nil {
		return err
	}

	finalize := func(result error) error {
		result = errors.Join(result, environment.Close())
		environment = nil
		if result != nil {
			result = errors.Join(result, storage.rollback())
		}

		return result
	}
	defer func() {
		if environment != nil {
			_ = finalize(nil)
		}
	}()
	info, err := environment.Stat()
	if err != nil {
		return finalize(fmt.Errorf("inspect environment file: %w", err))
	}

	exports := maps.Clone(values)
	maps.Copy(exports, files)
	encoded := encodeEnvironment(exports)

	for _, name := range slices.Sorted(maps.Keys(values)) {
		command := githubactions.Command{Name: "add-mask", Message: values[name]}
		if _, err := fmt.Fprintln(os.Stdout, command.String()); err != nil {
			return finalize(fmt.Errorf("register secret mask: %w", err))
		}
	}

	if err := storage.write(values, files); err != nil {
		return finalize(err)
	}

	if _, err := environment.WriteString(encoded); err != nil {
		return finalize(
			errors.Join(
				fmt.Errorf("write environment file: %w", err),
				environment.Truncate(info.Size()),
			),
		)
	}

	if err := environment.Sync(); err != nil {
		return finalize(
			errors.Join(
				fmt.Errorf("sync environment file: %w", err),
				environment.Truncate(info.Size()),
			),
		)
	}

	return finalize(nil)
}

func encodeEnvironment(values map[string]string) string {
	var output strings.Builder

	for _, name := range slices.Sorted(maps.Keys(values)) {
		value := values[name]

		delimiter := "tofu_workflows_" + rand.Text()
		for strings.Contains(value, delimiter) {
			delimiter = "tofu_workflows_" + rand.Text()
		}

		fmt.Fprintf(&output, "%s<<%s\n%s\n%s\n", name, delimiter, value, delimiter)
	}

	return output.String()
}
