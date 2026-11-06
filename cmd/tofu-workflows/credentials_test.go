package main_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const (
	backendValue  = "synthetic-backend-credential"
	providerValue = "synthetic-provider%credential\r\n_GitHubActionsFileCommandDelimeter_\nINJECTED=value\n"
	keyContents   = "-----BEGIN SYNTHETIC KEY-----\nsynthetic-private-key-bytes\n-----END SYNTHETIC KEY-----\n"
)

type credentialFixture struct {
	fixture
	runner      string
	environment string
	backend     string
	provider    string
}

func bundle(t *testing.T, values map[string]string) string {
	t.Helper()
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func newCredentials(t *testing.T) credentialFixture {
	t.Helper()
	f := credentialFixture{fixture: newFixture(t)}
	var err error
	f.runner, err = filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.environment = filepath.Join(f.directory, "github-env")
	write(t, f.directory, "github-env", nil)
	f.set.Validation.BackendSecretEnvNames = []string{"AWS_SECRET_ACCESS_KEY"}
	f.set.Validation.SecretEnvNames = []string{"PROVIDER_TOKEN", "SSH_KEY"}
	f.set.Validation.SecretFiles = map[string]string{"SSH_KEY": filepath.Join(f.runner, "tofu-workflows-credentials", "ssh", "id")}
	f.backend = bundle(t, map[string]string{"AWS_SECRET_ACCESS_KEY": backendValue})
	f.provider = bundle(t, map[string]string{"PROVIDER_TOKEN": providerValue, "SSH_KEY": keyContents})
	return f
}

func (f credentialFixture) command(t *testing.T, cleanup bool) *exec.Cmd {
	t.Helper()
	writeJSON(t, f.directory, "deployment.json", f.set)
	binary, err := filepath.Abs("../../dist/tofu-workflows")
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"credentials", "--checkout", f.directory, "--config", "deployment.json", "--environment", "qa"}
	if cleanup {
		args = append(args, "--cleanup")
	}
	command := exec.CommandContext(t.Context(), binary, args...)
	command.Dir = f.directory
	command.Env = []string{"RUNNER_TEMP=" + f.runner, "GITHUB_ENV=" + f.environment}
	if !cleanup {
		command.Env = append(command.Env, "TF_BACKEND_ENV="+f.backend, "TF_PROVIDER_ENV="+f.provider)
	}
	return command
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Expected path to be absent: %s; error=%v", path, err)
	}
}

func assertMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != mode {
		t.Fatalf("Mode for %s is %v; expected %v", path, info.Mode(), mode)
	}
}

func parseEnvironment(t *testing.T, encoded string) (map[string]string, []string) {
	t.Helper()
	values := make(map[string]string)
	var delimiters []string
	for encoded != "" {
		header, rest, found := strings.Cut(encoded, "\n")
		if !found {
			t.Fatal("Environment header does not end with a newline")
		}
		name, delimiter, found := strings.Cut(header, "<<")
		if !found || delimiter == "" {
			t.Fatal("Environment header does not specify a delimiter")
		}
		value, rest, found := strings.Cut(rest, "\n"+delimiter+"\n")
		if !found {
			t.Fatal("Environment value does not end with its delimiter")
		}
		if strings.Contains(value, delimiter) {
			t.Fatal("Environment delimiter occurs inside its value")
		}
		if _, exists := values[name]; exists {
			t.Fatal("Environment export repeats a name")
		}
		values[name] = value
		delimiters = append(delimiters, delimiter)
		encoded = rest
	}
	return values, delimiters
}

func TestCredentialsInstallAndCleanup(t *testing.T) {
	f := newCredentials(t)
	output, err := f.command(t, false).CombinedOutput()
	if err != nil {
		t.Fatalf("Credential installation failed: %s; %v", output, err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(output), "\n"), "\n") {
		if !strings.HasPrefix(line, "::add-mask::") {
			t.Fatalf("Unexpected credential output: %q", line)
		}
	}
	escaped := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(providerValue)
	if !strings.Contains(string(output), "::add-mask::"+escaped+"\n") {
		t.Fatal("Masking did not escape the runner command")
	}
	encoded := readFile(t, f.environment)
	values, delimiters := parseEnvironment(t, encoded)
	key := f.set.Validation.SecretFiles["SSH_KEY"]
	if len(values) != 3 || values["AWS_SECRET_ACCESS_KEY"] != backendValue || values["PROVIDER_TOKEN"] != providerValue || values["SSH_KEY"] != key {
		t.Fatal("Environment exports did not preserve the separate bundles and key pathname")
	}
	if strings.Contains(encoded, keyContents) || strings.Contains(encoded, "synthetic-private-key-bytes") {
		t.Fatal("Environment exports contain private key bytes")
	}
	if readFile(t, key) != keyContents {
		t.Fatal("Credential file bytes changed")
	}
	assertMode(t, key, 0o600)
	assertMode(t, filepath.Dir(key), os.ModeDir|0o700)
	assertMode(t, filepath.Join(f.runner, "tofu-workflows-credentials"), os.ModeDir|0o700)
	other := newCredentials(t)
	if output, err := other.command(t, false).CombinedOutput(); err != nil {
		t.Fatalf("Second install failed: %s; %v", output, err)
	}
	_, otherDelimiters := parseEnvironment(t, readFile(t, other.environment))
	for _, first := range delimiters {
		for _, second := range otherDelimiters {
			if first == second {
				t.Fatal("Installations reused an environment delimiter")
			}
		}
	}
	write(t, f.runner, "unrelated", []byte("preserve outside"))
	write(t, filepath.Dir(key), "unrelated", []byte("preserve inside"))
	if output, err := f.command(t, true).CombinedOutput(); err != nil || len(output) != 0 {
		t.Fatalf("Cleanup failed: %s; %v", output, err)
	}
	assertMissing(t, key)
	if readFile(t, filepath.Join(filepath.Dir(key), "unrelated")) != "preserve inside" || readFile(t, filepath.Join(f.runner, "unrelated")) != "preserve outside" {
		t.Fatal("Cleanup changed an unrelated file")
	}
	if output, err := other.command(t, true).CombinedOutput(); err != nil {
		t.Fatalf("Cleanup failed: %s; %v", output, err)
	}
	assertMissing(t, filepath.Join(other.runner, "tofu-workflows-credentials"))
}

func TestCredentialsRejectBundlesBeforeWrites(t *testing.T) {
	for _, test := range []struct{ name, backend, provider string }{
		{name: "missing backend", backend: "{}"},
		{name: "missing provider", provider: "{}"},
		{name: "duplicate", provider: `{"SSH_KEY":"secret-one","SSH_KEY":"secret-two","PROVIDER_TOKEN":"token"}`},
		{name: "unknown secret name", provider: `{"unknown-secret-name":"unknown-secret-value"}`},
		{name: "empty", provider: `{"SSH_KEY":"","PROVIDER_TOKEN":"token"}`},
		{name: "null", provider: `{"SSH_KEY":null,"PROVIDER_TOKEN":"token"}`},
		{name: "NUL", provider: `{"SSH_KEY":"nul\u0000secret","PROVIDER_TOKEN":"token"}`},
		{name: "object", provider: `{"SSH_KEY":{"secret":"value"},"PROVIDER_TOKEN":"token"}`},
		{name: "not JSON", provider: `private-key-not-json`},
		{name: "second document", provider: `{} {"secret":"value"}`},
		{name: "wrong bundle", backend: `{"PROVIDER_TOKEN":"misplaced"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCredentials(t)
			if test.backend != "" {
				f.backend = test.backend
			}
			if test.provider != "" {
				f.provider = test.provider
			}
			output, err := f.command(t, false).CombinedOutput()
			if err == nil {
				t.Fatal("Invalid bundle succeeded")
			}
			if strings.Contains(string(output), "::add-mask::") || strings.Contains(string(output), "secret-one") || strings.Contains(string(output), "unknown-secret") || strings.Contains(string(output), "private-key-not-json") {
				t.Fatalf("Invalid bundle disclosed input: %s", output)
			}
			if readFile(t, f.environment) != "" {
				t.Fatal("Invalid bundle changed the environment file")
			}
			assertMissing(t, filepath.Join(f.runner, "tofu-workflows-credentials"))
		})
	}
}

func TestCredentialsRejectForbiddenNames(t *testing.T) {
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "GITHUB_TOKEN", "ACTIONS_RUNTIME_TOKEN", "INPUT_TOKEN", "TF_CLI_ARGS_plan", "TF_LOG_PATH", "TERRAFORM_PRE_RUN"} {
		t.Run(name, func(t *testing.T) {
			f := newCredentials(t)
			f.set.Validation.SecretEnvNames = append(f.set.Validation.SecretEnvNames, name)
			f.provider = bundle(t, map[string]string{"PROVIDER_TOKEN": providerValue, "SSH_KEY": keyContents, name: "forbidden-value"})
			output, err := f.command(t, false).CombinedOutput()
			if err == nil || !strings.Contains(string(output), "not allowed") || strings.Contains(string(output), "forbidden-value") {
				t.Fatalf("Forbidden name did not fail safely: %s; %v", output, err)
			}
			assertMissing(t, filepath.Join(f.runner, "tofu-workflows-credentials"))
		})
	}
}

func TestCredentialsRejectFileBindingsBeforeWrites(t *testing.T) {
	for _, name := range []string{"outside", "relative", "duplicate", "file ancestor", "existing", "directory", "FIFO", "target link", "ancestor link", "directory permissions", "undeclared", "overlapping bundles", "environment file missing"} {
		t.Run(name, func(t *testing.T) {
			f := newCredentials(t)
			key := f.set.Validation.SecretFiles["SSH_KEY"]
			if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "outside":
				f.set.Validation.SecretFiles["SSH_KEY"] = filepath.Join(f.runner, "outside")
			case "relative":
				f.set.Validation.SecretFiles["SSH_KEY"] = "relative"
			case "duplicate":
				f.set.Validation.SecretFiles["PROVIDER_TOKEN"] = key
			case "file ancestor":
				f.set.Validation.SecretFiles["PROVIDER_TOKEN"] = key + "/child"
			case "existing":
				write(t, filepath.Dir(key), "id", []byte("preserve existing"))
			case "directory":
				if err := os.Mkdir(key, 0o700); err != nil {
					t.Fatal(err)
				}
			case "FIFO":
				if err := syscall.Mkfifo(key, 0o600); err != nil {
					t.Fatal(err)
				}
			case "target link":
				if err := os.Symlink(f.environment, key); err != nil {
					t.Fatal(err)
				}
			case "ancestor link":
				link := filepath.Join(f.runner, "tofu-workflows-credentials", "link")
				if err := os.Symlink(filepath.Dir(key), link); err != nil {
					t.Fatal(err)
				}
				f.set.Validation.SecretFiles["SSH_KEY"] = filepath.Join(link, "id")
			case "directory permissions":
				if err := os.Chmod(filepath.Dir(key), 0o755); err != nil {
					t.Fatal(err)
				}
			case "undeclared":
				f.set.Validation.SecretFiles["UNDECLARED"] = key
			case "overlapping bundles":
				f.set.Validation.BackendSecretEnvNames = append(f.set.Validation.BackendSecretEnvNames, "SSH_KEY")
			case "environment file missing":
				f.environment = filepath.Join(f.directory, "missing-environment")
			}
			output, err := f.command(t, false).CombinedOutput()
			if err == nil || strings.Contains(string(output), "::add-mask::") {
				t.Fatalf("Unsafe file binding did not fail before masking: %s; %v", output, err)
			}
			if name != "environment file missing" && readFile(t, f.environment) != "" {
				t.Fatal("Invalid binding changed the environment file")
			}
			if name == "existing" && readFile(t, key) != "preserve existing" {
				t.Fatal("Installation changed an existing target")
			}
		})
	}
}

func TestCredentialsCleanupPreservesReplacement(t *testing.T) {
	f := newCredentials(t)
	if output, err := f.command(t, false).CombinedOutput(); err != nil {
		t.Fatalf("Install failed: %s; %v", output, err)
	}
	key := f.set.Validation.SecretFiles["SSH_KEY"]
	if err := os.Rename(key, key+".original"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Dir(key), "id", []byte("replacement"))
	output, err := f.command(t, true).CombinedOutput()
	if err == nil || !strings.Contains(string(output), "ownership changed") {
		t.Fatalf("Cleanup did not reject replacement: %s; %v", output, err)
	}
	if readFile(t, key) != "replacement" || readFile(t, key+".original") != keyContents {
		t.Fatal("Cleanup removed an unowned file")
	}
}

func TestCredentialsMaskFailurePreventsWrites(t *testing.T) {
	f := newCredentials(t)
	command := f.command(t, false)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	command.Stdout = writer
	var diagnostic bytes.Buffer
	command.Stderr = &diagnostic
	if err := command.Run(); err == nil {
		t.Fatal("Installation succeeded after masking failed")
	}
	if readFile(t, f.environment) != "" {
		t.Fatal("Installation exported credentials before masking succeeded")
	}
	assertMissing(t, filepath.Join(f.runner, "tofu-workflows-credentials"))
}

func TestCredentialsRequireExplicitEnvironment(t *testing.T) {
	f := newCredentials(t)
	command := f.command(t, false)
	command.Args[len(command.Args)-1] = "missing"
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "--environment must match") {
		t.Fatalf("Unknown environment did not fail: %s; %v", output, err)
	}
	assertMissing(t, filepath.Join(f.runner, "tofu-workflows-credentials"))
	f.set.Production.BackendSecretEnvNames = []string{"PRODUCTION_TOKEN"}
	f.set.Production.SecretEnvNames = nil
	f.backend = bundle(t, map[string]string{"PRODUCTION_TOKEN": backendValue})
	f.provider = "{}"
	command = f.command(t, false)
	command.Args[len(command.Args)-1] = "production"
	output, err = command.CombinedOutput()
	if err != nil {
		t.Fatalf("Explicit production selection failed: %s; %v", output, err)
	}
	values, _ := parseEnvironment(t, readFile(t, f.environment))
	if len(values) != 1 || values["PRODUCTION_TOKEN"] != backendValue {
		t.Fatalf("Selected environment exported %d values", len(values))
	}
}

func TestCredentialsRollbackPreservesConcurrentTarget(t *testing.T) {
	f := newCredentials(t)
	key := f.set.Validation.SecretFiles["SSH_KEY"]
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	backendPath := filepath.Join(f.runner, "tofu-workflows-credentials", "backend", "key")
	f.set.Validation.SecretFiles["AWS_SECRET_ACCESS_KEY"] = backendPath
	f.provider = bundle(t, map[string]string{"PROVIDER_TOKEN": strings.Repeat("x", 120*1024), "SSH_KEY": keyContents})
	command := f.command(t, false)
	reader, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var diagnostic bytes.Buffer
	command.Stderr = &diagnostic
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	firstMask := make([]byte, len("::add-mask::"+backendValue+"\n"))
	if _, err := io.ReadFull(reader, firstMask); err != nil {
		t.Fatal(err)
	}
	if string(firstMask) != "::add-mask::"+backendValue+"\n" {
		t.Fatal("Installation did not register its first mask")
	}
	if readFile(t, f.environment) != "" {
		t.Fatal("Environment exports preceded the remaining secret masks")
	}
	write(t, filepath.Dir(key), "id", []byte("concurrent target"))
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("Installation replaced a concurrent target")
	}
	if !strings.Contains(diagnostic.String(), "file exists") {
		t.Fatalf("Unexpected install error: %s", diagnostic.String())
	}
	if readFile(t, key) != "concurrent target" || readFile(t, f.environment) != "" {
		t.Fatal("Rollback changed an unowned file or exported credentials")
	}
	assertMissing(t, filepath.Dir(backendPath))
}

func TestCredentialsRejectUnsafeCleanupReceipt(t *testing.T) {
	for _, change := range []string{"missing bindings", "unknown field", "symlink", "permissions", "outside path"} {
		t.Run(change, func(t *testing.T) {
			f := newCredentials(t)
			if output, err := f.command(t, false).CombinedOutput(); err != nil {
				t.Fatalf("Install failed: %s; %v", output, err)
			}
			receipts, err := filepath.Glob(filepath.Join(f.runner, "tofu-workflows-credentials", ".ownership-*.json"))
			if err != nil || len(receipts) != 1 {
				t.Fatalf("Expected one credential ownership receipt: %v", err)
			}
			receipt := receipts[0]
			key := f.set.Validation.SecretFiles["SSH_KEY"]
			switch change {
			case "missing bindings":
				write(t, filepath.Dir(receipt), filepath.Base(receipt), []byte("[]"))
			case "unknown field":
				write(t, filepath.Dir(receipt), filepath.Base(receipt), []byte(`[{"unexpected":true}]`))
			case "symlink":
				if err := os.Rename(receipt, receipt+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(receipt+".original", receipt); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.Chmod(receipt, 0o644); err != nil {
					t.Fatal(err)
				}
			case "outside path":
				write(t, filepath.Dir(receipt), filepath.Base(receipt), []byte(`[{"path":"../outside","identity":"1:1","directory":false}]`))
			}
			output, err := f.command(t, true).CombinedOutput()
			if err == nil {
				t.Fatal("Cleanup accepted an unsafe receipt")
			}
			if strings.Contains(string(output), "synthetic-private-key-bytes") {
				t.Fatal("Cleanup disclosed key bytes")
			}
			if readFile(t, key) != keyContents {
				t.Fatal("Unsafe cleanup removed the credential file")
			}
		})
	}
}
