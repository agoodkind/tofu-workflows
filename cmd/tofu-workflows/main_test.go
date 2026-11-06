package main_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agoodkind/tofu-workflows/internal/config"
	"github.com/agoodkind/tofu-workflows/internal/release"
)

const (
	revision     = "1234567890123456789012345678901234567890"
	image        = "ghcr.io/example/application@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	lockContents = "# Synthetic provider lock for file validation.\n"
)

type fixture struct {
	directory string
	set       config.DeploymentSet
	manifest  release.Manifest
}

func write(t *testing.T, directory, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeJSON[T any](t *testing.T, directory, name string, value T) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	write(t, directory, name, data)
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	directory := t.TempDir()
	for _, name := range []string{"qa", "production"} {
		if err := os.Mkdir(filepath.Join(directory, name), 0o700); err != nil {
			t.Fatal(err)
		}
		write(t, directory, name+"/main.tf", []byte("terraform {\n  backend \"s3\" {}\n}\n"))
		write(t, directory, name+"/.terraform.lock.hcl", []byte(lockContents))
		write(t, directory, name+"/backend.hcl", []byte(fmt.Sprintf("endpoints = { s3 = \"https://state.example.test\" }\nbucket = \"application-state\"\nkey = %q\n", name+".tfstate")))
		write(t, directory, name+"/values.tfvars.json", []byte("{}"))
	}
	environment := func(name string) config.Environment {
		return config.Environment{
			Name: name, GitHubPlanEnvironment: name + "-plan", GitHubApplyEnvironment: name + "-apply",
			Configuration: config.Configuration{Repository: "example/configuration", Revision: revision, Root: name, TofuVersion: config.TofuVersion, ProviderLockDigest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(lockContents)))},
			CLIWorkspace:  "default", BackendFile: name + "/backend.hcl", StateID: name + "-label", Parallelism: 1, JobTimeoutMinutes: 60,
			VariableFiles: []string{name + "/values.tfvars.json"}, HealthCheck: config.Check{Image: image, TimeoutSeconds: 60}, InspectCheck: config.Check{Image: image, TimeoutSeconds: 60},
		}
	}
	return fixture{directory: directory, set: config.DeploymentSet{
		Version: 1, ID: "application", SourceBranch: "main", InitialCommit: revision, ProducerWorkflowID: 17, RecorderActorID: 19, DispatchWorkflow: "deploy.yml",
		RequiredChecks: []config.CheckIdentity{{AppID: 23, Name: "Build"}}, ArtifactRepositories: map[string]string{"application": "ghcr.io/example/application"},
		Variables:  map[string]config.VariableRule{"channel": {Kind: "string", AllowedValues: []string{"stable"}}, "generation": {Kind: "number"}, "enabled": {Kind: "bool"}},
		Validation: environment("qa"), Production: environment("production"),
	}, manifest: release.Manifest{Version: 1, SourceSHA: revision, BuildRunID: 29, BuildAttempt: 2, Artifacts: map[string]string{"application": image}, Variables: map[string]json.RawMessage{"channel": json.RawMessage(`"stable"`), "generation": json.RawMessage(`9007199254740993`), "enabled": json.RawMessage(`true`)}}}
}

func invoke(t *testing.T, directory string, args ...string) (string, error) {
	t.Helper()
	binary, err := filepath.Abs("../../dist/tofu-workflows")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), binary, args...)
	command.Dir = directory
	command.Env = []string{"HOME=" + directory, "TMPDIR=" + directory}
	output, err := command.CombinedOutput()
	return string(output), err
}

func assertResult(t *testing.T, output string, err error, diagnostic string) {
	t.Helper()
	if diagnostic == "" {
		if err != nil || output != "Consumer inputs are valid.\n" {
			t.Fatalf("Expected successful validation; output=%q error=%v", output, err)
		}
		return
	}
	if err == nil || !strings.Contains(output, diagnostic) {
		t.Fatalf("Expected failure containing %q; output=%q error=%v", diagnostic, output, err)
	}
}

func TestValidateConsumerInputs(t *testing.T) {
	for _, test := range []struct {
		name       string
		change     func(*testing.T, *fixture)
		diagnostic string
	}{
		{name: "complete inputs"},
		{name: "missing id", change: func(_ *testing.T, f *fixture) { f.set.ID = "" }, diagnostic: "id is required"},
		{name: "missing check", change: func(_ *testing.T, f *fixture) { f.set.Validation.HealthCheck = config.Check{} }, diagnostic: "check.image"},
		{name: "revision", change: func(_ *testing.T, f *fixture) { f.set.Validation.Configuration.Revision = "main" }, diagnostic: "configuration.revision"},
		{name: "digest", change: func(_ *testing.T, f *fixture) {
			f.manifest.Artifacts["application"] = "ghcr.io/example/application:latest"
		}, diagnostic: "sha256 digest"},
		{name: "parallelism", change: func(_ *testing.T, f *fixture) { f.set.Validation.Parallelism = 0 }, diagnostic: "parallelism"},
		{name: "timeout", change: func(_ *testing.T, f *fixture) { f.set.Validation.JobTimeoutMinutes = 181 }, diagnostic: "job_timeout_minutes"},
		{name: "undeclared variable", change: func(_ *testing.T, f *fixture) { f.manifest.Variables["root"] = json.RawMessage(`"other"`) }, diagnostic: "not declared"},
		{name: "wrong scalar kind", change: func(_ *testing.T, f *fixture) { f.manifest.Variables["enabled"] = json.RawMessage(`"true"`) }, diagnostic: "must have kind bool"},
		{name: "forbidden string", change: func(_ *testing.T, f *fixture) { f.manifest.Variables["channel"] = json.RawMessage(`"arbitrary"`) }, diagnostic: "allowed_values"},
		{name: "null scalar", change: func(_ *testing.T, f *fixture) { f.manifest.Variables["enabled"] = json.RawMessage(`null`) }, diagnostic: "string, number, or bool"},
		{name: "object scalar", change: func(_ *testing.T, f *fixture) { f.manifest.Variables["enabled"] = json.RawMessage(`{}`) }, diagnostic: "string, number, or bool"},
		{name: "array scalar", change: func(_ *testing.T, f *fixture) { f.manifest.Variables["enabled"] = json.RawMessage(`[]`) }, diagnostic: "string, number, or bool"},
		{name: "repository allowlist", change: func(_ *testing.T, f *fixture) {
			f.manifest.Artifacts["application"] = strings.Replace(image, "example", "other", 1)
		}, diagnostic: "protected repository"},
		{name: "duplicate states with different labels", change: func(t *testing.T, f *fixture) {
			write(t, f.directory, "production/backend.hcl", []byte("endpoint = \"https://STATE.example.test:443/\"\nbucket = \"application-state\"\nkey = \"qa.tfstate\"\n"))
		}, diagnostic: "duplicate physical state identity"},
		{name: "named workspace collides with default", change: func(t *testing.T, f *fixture) {
			f.set.Validation.CLIWorkspace = "qa"
			write(t, f.directory, "production/backend.hcl", []byte("endpoint = \"https://state.example.test/\"\nbucket = \"application-state\"\nkey = \"env:/qa/qa.tfstate\"\n"))
		}, diagnostic: "duplicate physical state identity"},
		{name: "different workspace identities", change: func(t *testing.T, f *fixture) {
			f.set.Validation.CLIWorkspace = "qa"
			f.set.Production.CLIWorkspace = "production"
			write(t, f.directory, "production/backend.hcl", []byte("endpoint = \"https://state.example.test\"\nbucket = \"application-state\"\nkey = \"qa.tfstate\"\n"))
		}},
		{name: "traversal root", change: func(_ *testing.T, f *fixture) { f.set.Validation.Configuration.Root = "../outside" }, diagnostic: "beneath the checkout"},
		{name: "symlink escape", change: func(t *testing.T, f *fixture) {
			outside := t.TempDir()
			write(t, outside, "backend.hcl", []byte("sensitive sentinel"))
			if err := os.Symlink(filepath.Join(outside, "backend.hcl"), filepath.Join(f.directory, "outside.hcl")); err != nil {
				t.Fatal(err)
			}
			f.set.Validation.BackendFile = "outside.hcl"
		}, diagnostic: "outside the checkout"},
		{name: "lock digest mismatch", change: func(t *testing.T, f *fixture) { write(t, f.directory, "qa/.terraform.lock.hcl", []byte("changed")) }, diagnostic: "provider lock digest"},
		{name: "backend type", change: func(t *testing.T, f *fixture) {
			write(t, f.directory, "qa/main.tf", []byte("terraform {\n backend \"local\" {}\n}\n"))
		}, diagnostic: "s3 backend"},
		{name: "backend expression", change: func(t *testing.T, f *fixture) {
			write(t, f.directory, "qa/backend.hcl", []byte("endpoint = var.endpoint\nbucket = \"application-state\"\nkey = \"qa.tfstate\"\n"))
		}, diagnostic: "literal string"},
		{name: "tofu file overrides tf backend", change: func(t *testing.T, f *fixture) {
			write(t, f.directory, "qa/main.tofu", []byte("terraform {\n backend \"local\" {}\n}\n"))
		}, diagnostic: "s3 backend"},
		{name: "tofu file replaces ignored tf source", change: func(t *testing.T, f *fixture) {
			write(t, f.directory, "qa/main.tf", []byte("invalid HCL that OpenTofu ignores"))
			write(t, f.directory, "qa/main.tofu", []byte("terraform {\n backend \"s3\" {}\n}\n"))
		}},
		{name: "missing variable file", change: func(_ *testing.T, f *fixture) {
			f.set.Validation.VariableFiles = []string{"missing.tfvars"}
		}, diagnostic: "variable file"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			if test.change != nil {
				test.change(t, &f)
			}
			writeJSON(t, f.directory, "deployment.json", f.set)
			writeJSON(t, f.directory, "release.json", f.manifest)
			output, err := invoke(t, f.directory, "validate", "--checkout", f.directory, "--config", "deployment.json", "--manifest", "release.json", "--expected-run-id", "29", "--expected-attempt", "2")
			assertResult(t, output, err, test.diagnostic)
		})
	}
	t.Run("unknown configuration field", func(t *testing.T) {
		f := newFixture(t)
		write(t, f.directory, "deployment.json", []byte(`{"version":1,"unexpected":true}`))
		output, err := invoke(t, f.directory, "validate", "--checkout", f.directory, "--config", "deployment.json")
		assertResult(t, output, err, `unknown field "unexpected"`)
	})
	t.Run("command boundary", func(t *testing.T) {
		f := newFixture(t)
		output, err := invoke(t, f.directory, "deploy")
		assertResult(t, output, err, "Unsupported command")
		output, err = invoke(t, f.directory, "validate")
		assertResult(t, output, err, "--checkout and --config are required")
	})
}

func archiveBytes(t *testing.T, names []string, mode os.FileMode, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, name := range names {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(mode)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if mode.IsDir() {
			continue
		}
		if _, err := entry.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestRejectUnsafeManifestArchive(t *testing.T) {
	for _, test := range []struct {
		name       string
		names      []string
		mode       os.FileMode
		change     func([]byte) []byte
		diagnostic string
	}{
		{name: "valid archive", names: []string{"release.json"}},
		{name: "exactly 64 KiB", names: []string{"release.json"}, change: func(data []byte) []byte { return append(data, bytes.Repeat([]byte(" "), 65536-len(data))...) }},
		{name: "traversal", names: []string{"../outside.json"}, diagnostic: "regular release.json"},
		{name: "absolute path", names: []string{"/release.json"}, diagnostic: "regular release.json"},
		{name: "symlink", names: []string{"release.json"}, mode: os.ModeSymlink, diagnostic: "regular release.json"},
		{name: "directory", names: []string{"release.json/"}, mode: os.ModeDir, diagnostic: "regular release.json"},
		{name: "duplicate entry", names: []string{"release.json", "release.json"}, diagnostic: "exactly one"},
		{name: "extra entry", names: []string{"release.json", "extra.json"}, diagnostic: "exactly one"},
		{name: "forged run", names: []string{"release.json"}, change: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"build_run_id":29`), []byte(`"build_run_id":30`), 1)
		}, diagnostic: "expected build run"},
		{name: "forged attempt", names: []string{"release.json"}, change: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"build_attempt":2`), []byte(`"build_attempt":3`), 1)
		}, diagnostic: "expected build run"},
		{name: "oversized entry", names: []string{"release.json"}, change: func(data []byte) []byte { return append(data, bytes.Repeat([]byte(" "), 65536)...) }, diagnostic: "64 KiB"},
		{name: "trailing document", names: []string{"release.json"}, change: func(data []byte) []byte { return append(data, []byte(" {}")...) }, diagnostic: "after top-level value"},
		{name: "unexpected manifest key", names: []string{"release.json"}, change: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"version":1`), []byte(`"version":1,"configuration_revision":"main"`), 1)
		}, diagnostic: "unknown field"},
		{name: "duplicate JSON key", names: []string{"release.json"}, change: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"version":1`), []byte(`"version":2,"version":1`), 1)
		}, diagnostic: "duplicate field"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			writeJSON(t, f.directory, "deployment.json", f.set)
			data, err := json.Marshal(f.manifest)
			if err != nil {
				t.Fatal(err)
			}
			if test.change != nil {
				data = test.change(data)
			}
			write(t, f.directory, "release.zip", archiveBytes(t, test.names, test.mode, data))
			parent := filepath.Dir(f.directory)
			const sentinel = "Existing external file must remain unchanged."
			write(t, parent, "outside.json", []byte(sentinel))
			before, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			output, err := invoke(t, f.directory, "validate", "--checkout", f.directory, "--config", "deployment.json", "--artifact", "release.zip", "--expected-run-id", "29", "--expected-attempt", "2")
			assertResult(t, output, err, test.diagnostic)
			after, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			if len(before) != len(after) {
				t.Fatal("Validation wrote outside the checkout.")
			}
			for index := range before {
				if before[index].Name() != after[index].Name() {
					t.Fatal("Validation changed a file outside the checkout.")
				}
			}
			external, err := os.ReadFile(filepath.Join(parent, "outside.json"))
			if err != nil {
				t.Fatal(err)
			}
			if string(external) != sentinel {
				t.Fatal("Validation overwrote a file outside the checkout.")
			}
		})
	}
}

func TestRejectCaseVariantJSONFields(t *testing.T) {
	for _, test := range []struct {
		name        string
		config      bool
		original    string
		replacement string
	}{
		{name: "configuration alias", config: true, original: `"producer_workflow_id":17`, replacement: `"PRODUCER_WORKFLOW_ID":17`},
		{name: "configuration conflict", config: true, original: `"producer_workflow_id":17`, replacement: `"producer_workflow_id":18,"PRODUCER_WORKFLOW_ID":17`},
		{name: "nested configuration alias", config: true, original: `"root":"qa"`, replacement: `"ROOT":"qa"`},
		{name: "nested configuration conflict", config: true, original: `"root":"qa"`, replacement: `"root":"missing","ROOT":"qa"`},
		{name: "variable rule alias", config: true, original: `"kind":"bool"`, replacement: `"KIND":"bool"`},
		{name: "variable rule conflict", config: true, original: `"kind":"bool"`, replacement: `"kind":"string","KIND":"bool"`},
		{name: "manifest alias", original: `"build_run_id":29`, replacement: `"BUILD_RUN_ID":29`},
		{name: "manifest conflict", original: `"build_run_id":29`, replacement: `"build_run_id":30,"BUILD_RUN_ID":29`},
	} {
		for _, archive := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/archive=%t", test.name, archive), func(t *testing.T) {
				f := newFixture(t)
				configuration, err := json.Marshal(f.set)
				if err != nil {
					t.Fatal(err)
				}
				manifest, err := json.Marshal(f.manifest)
				if err != nil {
					t.Fatal(err)
				}
				if test.config {
					configuration = bytes.Replace(configuration, []byte(test.original), []byte(test.replacement), 1)
				} else {
					manifest = bytes.Replace(manifest, []byte(test.original), []byte(test.replacement), 1)
				}
				write(t, f.directory, "deployment.json", configuration)
				arguments := []string{"validate", "--checkout", f.directory, "--config", "deployment.json", "--expected-run-id", "29", "--expected-attempt", "2"}
				if archive {
					write(t, f.directory, "release.zip", archiveBytes(t, []string{"release.json"}, 0, manifest))
					arguments = append(arguments, "--artifact", "release.zip")
				} else {
					write(t, f.directory, "release.json", manifest)
					arguments = append(arguments, "--manifest", "release.json")
				}
				output, err := invoke(t, f.directory, arguments...)
				assertResult(t, output, err, "unknown field")
			})
		}
	}
}

func TestValidateCaseSensitiveMaps(t *testing.T) {
	f := newFixture(t)
	f.set.Variables["CHANNEL"] = config.VariableRule{Kind: "string", AllowedValues: []string{"preview"}}
	f.set.Variables["large_number"] = config.VariableRule{Kind: "number"}
	f.manifest.Variables["CHANNEL"] = json.RawMessage(`"preview"`)
	f.manifest.Variables["large_number"] = json.RawMessage(`1e10000`)
	f.set.ArtifactRepositories["Application"] = "ghcr.io/example/other"
	f.manifest.Artifacts["Application"] = strings.Replace(image, "/application@", "/other@", 1)
	writeJSON(t, f.directory, "deployment.json", f.set)
	writeJSON(t, f.directory, "release.json", f.manifest)
	output, err := invoke(t, f.directory, "validate", "--checkout", f.directory, "--config", "deployment.json", "--manifest", "release.json", "--expected-run-id", "29", "--expected-attempt", "2")
	assertResult(t, output, err, "")
	loaded, err := release.Load(filepath.Join(f.directory, "release.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"generation", "large_number", "channel", "CHANNEL"} {
		if !bytes.Equal(loaded.Variables[name], f.manifest.Variables[name]) {
			t.Fatalf("Manifest variable %q changed during decoding.", name)
		}
	}
}
