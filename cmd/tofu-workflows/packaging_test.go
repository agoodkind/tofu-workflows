//go:build packaging

package main_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agoodkind/tofu-workflows/internal/config"
)

const (
	composeFile        = "../../compose.yml"
	composeService     = "action"
	composeProjectEnv  = "COMPOSE_PROJECT_NAME"
	composeProjectKey  = "com.docker.compose.project"
	containerWorkspace = "/github/workspace"
	containerRunner    = "/github/runner_temp"
	cleanupTimeout     = 3 * time.Minute
	cloudflaredVersion = "cloudflared version 2026.9.1 "
	injectedName       = "injected"
	unsafeConfigName   = "deployment sets/$(mkdir injected); `mkdir injected` & mkdir injected | 'single' \"double\".json"
)

type composeProject struct {
	name  string
	image string
}

type containerCall struct {
	entrypoint  string
	checkout    string
	environment map[string]string
}

type containerResult struct {
	stdout string
	stderr string
	err    error
}

func newComposeProject(t *testing.T) composeProject {
	t.Helper()
	name := os.Getenv(composeProjectEnv)
	if name == "" {
		t.Fatalf(
			"Set %s to a unique Compose project name before running the packaging test.",
			composeProjectEnv,
		)
	}
	project := composeProject{name: name}
	output, err := project.command(
		t.Context(),
		"config",
		"--images",
		composeService,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("Image listing failed: %s; %v", output, err)
	}
	project.image = strings.TrimSpace(string(output))
	if leftover := project.ownedResources(t.Context()); leftover != "" {
		t.Fatalf("Compose project %s already owns Docker resources: %s", name, leftover)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		output, err := project.command(
			ctx,
			"down",
			"--volumes",
			"--remove-orphans",
			"--rmi",
			"local",
		).CombinedOutput()
		if err != nil {
			t.Errorf("Compose cleanup failed for project %s: %s; %v", name, output, err)
		}
		if leftover := project.ownedResources(ctx); leftover != "" {
			t.Errorf("Compose cleanup left resources for project %s: %s", name, leftover)
		}
	})
	return project
}

func (project composeProject) command(ctx context.Context, args ...string) *exec.Cmd {
	composeArgs := append(
		[]string{"compose", "--project-name", project.name, "--file", composeFile},
		args...,
	)
	return exec.CommandContext(ctx, "docker", composeArgs...)
}

func (project composeProject) labelFilter() string {
	return "label=" + composeProjectKey + "=" + project.name
}

func (project composeProject) ownedResources(ctx context.Context) string {
	var leftover []string
	for _, kind := range [][]string{
		{"container", "ls", "--all", "--filter", project.labelFilter()},
		{"network", "ls", "--filter", project.labelFilter()},
		{"volume", "ls", "--filter", project.labelFilter()},
		{"image", "ls", "--filter", "reference=" + project.image},
	} {
		args := append(append([]string{}, kind...), "--quiet")
		output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			leftover = append(
				leftover,
				kind[0]+" listing failed: "+strings.TrimSpace(string(output))+"; "+err.Error(),
			)
			continue
		}
		if identifiers := strings.Fields(string(output)); len(identifiers) != 0 {
			leftover = append(leftover, kind[0]+" "+strings.Join(identifiers, " "))
		}
	}
	return strings.Join(leftover, "; ")
}

func (project composeProject) build(t *testing.T) {
	t.Helper()
	output, err := project.command(t.Context(), "build", composeService).CombinedOutput()
	if err != nil {
		t.Fatalf("Compose build failed: %s; %v", output, err)
	}
	output, err = exec.CommandContext(
		t.Context(),
		"docker",
		"image",
		"inspect",
		"--format",
		"{{.Id}}",
		project.image,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("Image listing failed: %s; %v", output, err)
	}
	t.Logf("Built image: %s %s", project.image, strings.TrimSpace(string(output)))
}

func (project composeProject) run(
	t *testing.T,
	call containerCall,
	args ...string,
) containerResult {
	t.Helper()
	runArgs := []string{"run", "--rm", "--no-TTY"}
	if call.entrypoint != "" {
		runArgs = append(runArgs, "--entrypoint", call.entrypoint)
	}
	if call.checkout != "" {
		source, err := filepath.EvalSymlinks(call.checkout)
		if err != nil {
			t.Fatal(err)
		}
		runArgs = append(runArgs, "--volume", source+":"+containerWorkspace)
	}
	environment := os.Environ()
	for name, value := range call.environment {
		runArgs = append(runArgs, "--env", name)
		environment = append(environment, name+"="+value)
	}
	runArgs = append(runArgs, composeService)
	runArgs = append(runArgs, args...)
	command := project.command(t.Context(), runArgs...)
	command.Env = environment
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return containerResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

func actionArguments(checkout, configuration, manifest, artifact, runID, attempt string) []string {
	return []string{
		"validate",
		"--checkout=" + checkout,
		"--config=" + configuration,
		"--manifest=" + manifest,
		"--artifact=" + artifact,
		"--expected-run-id=" + runID,
		"--expected-attempt=" + attempt,
	}
}

func assertContainerResult(t *testing.T, result containerResult, diagnostic string) {
	t.Helper()
	if diagnostic == "" {
		if result.err != nil || result.stdout != "Consumer inputs are valid.\n" {
			t.Fatalf(
				"The validation command failed or returned an unexpected response: stdout=%q stderr=%q error=%v.",
				result.stdout,
				result.stderr,
				result.err,
			)
		}
		return
	}
	if result.err == nil || !strings.Contains(result.stderr, diagnostic) {
		t.Fatalf(
			"The validation command did not reject the input with %q: stdout=%q stderr=%q error=%v.",
			diagnostic,
			result.stdout,
			result.stderr,
			result.err,
		)
	}
}

func TestActionContainer(t *testing.T) {
	project := newComposeProject(t)
	project.build(t)
	t.Run(
		"executables",
		func(t *testing.T) {
			result := project.run(t, containerCall{})
			if result.err == nil || !strings.Contains(result.stderr, "A command is required.") {
				t.Fatalf(
					"Default entrypoint is not the helper; stdout=%q stderr=%q error=%v",
					result.stdout,
					result.stderr,
					result.err,
				)
			}
			result = project.run(t, containerCall{entrypoint: "tofu"}, "version")
			if result.err != nil ||
				!strings.HasPrefix(result.stdout, "OpenTofu v"+config.TofuVersion+"\n") {
				t.Fatalf(
					"The packaged OpenTofu command failed or did not report version %s: stdout=%q stderr=%q error=%v.",
					config.TofuVersion,
					result.stdout,
					result.stderr,
					result.err,
				)
			}
			result = project.run(t, containerCall{entrypoint: "cloudflared"}, "--version")
			if result.err != nil || !strings.HasPrefix(result.stdout, cloudflaredVersion) {
				t.Fatalf(
					"The packaged cloudflared command failed or did not report %q: stdout=%q stderr=%q error=%v.",
					cloudflaredVersion,
					result.stdout,
					result.stderr,
					result.err,
				)
			}
		},
	)
	t.Run(
		"validate",
		func(t *testing.T) {
			for _, test := range []struct {
				name, configuration, manifest, artifact, runID, attempt, diagnostic string
			}{
				{
					name:          "configuration only",
					configuration: "deployment.json",
					runID:         "0",
					attempt:       "0",
				},
				{
					name:          "manifest",
					configuration: "deployment.json",
					manifest:      "release.json",
					runID:         "29",
					attempt:       "2",
				},
				{
					name:          "artifact",
					configuration: "deployment.json",
					artifact:      "release.zip",
					runID:         "29",
					attempt:       "2",
				},
				{
					name:          "unsafe configuration path",
					configuration: unsafeConfigName,
					runID:         "0",
					attempt:       "0",
				},
				{
					name:       "missing configuration",
					runID:      "0",
					attempt:    "0",
					diagnostic: "--checkout and --config are required",
				},
				{
					name:          "release without identity",
					configuration: "deployment.json",
					manifest:      "release.json",
					runID:         "0",
					attempt:       "0",
					diagnostic:    "--expected-run-id and --expected-attempt are required for release input",
				},
				{
					name:          "manifest and artifact",
					configuration: "deployment.json",
					manifest:      "release.json",
					artifact:      "release.zip",
					runID:         "29",
					attempt:       "2",
					diagnostic:    "select either --manifest or --artifact",
				},
				{
					name:          "forged artifact run",
					configuration: "deployment.json",
					artifact:      "release.zip",
					runID:         "30",
					attempt:       "2",
					diagnostic:    "expected build run",
				},
			} {
				t.Run(
					test.name,
					func(t *testing.T) {
						f := newFixture(t)
						writeJSON(t, f.directory, "deployment.json", f.set)
						if err := os.MkdirAll(
							filepath.Join(f.directory, filepath.Dir(unsafeConfigName)),
							0o700,
						); err != nil {
							t.Fatal(err)
						}
						writeJSON(t, f.directory, unsafeConfigName, f.set)
						writeJSON(t, f.directory, "release.json", f.manifest)
						manifest, err := json.Marshal(f.manifest)
						if err != nil {
							t.Fatal(err)
						}
						write(
							t,
							f.directory,
							"release.zip",
							archiveBytes(t, []string{"release.json"}, 0, manifest),
						)
						args := actionArguments(
							".",
							test.configuration,
							test.manifest,
							test.artifact,
							test.runID,
							test.attempt,
						)
						result := project.run(t, containerCall{checkout: f.directory}, args...)
						assertContainerResult(t, result, test.diagnostic)
						if _, err := os.Lstat(filepath.Join(f.directory, injectedName)); !errors.Is(
							err,
							os.ErrNotExist,
						) {
							t.Fatalf(
								"Validation ran a shell command from an argument; error=%v",
								err,
							)
						}
					},
				)
			}
		},
	)
	t.Run(
		"credentials",
		func(t *testing.T) {
			f := newCredentials(t)
			credentialDirectory := filepath.Join(containerRunner, "tofu-workflows-credentials")
			keyDirectory := filepath.Join(credentialDirectory, "ssh")
			key := filepath.Join(keyDirectory, "id")
			f.set.Validation.SecretFiles = map[string]string{"SSH_KEY": key}
			writeJSON(t, f.directory, "deployment.json", f.set)
			credentialArgs := []string{
				"credentials",
				"--checkout=.",
				"--config=deployment.json",
				"--environment=qa",
			}
			runnerEnvironment := map[string]string{
				"RUNNER_TEMP": containerRunner,
				"GITHUB_ENV":  containerWorkspace + "/github-env",
			}
			installEnvironment := map[string]string{
				"TF_BACKEND_ENV":  f.backend,
				"TF_PROVIDER_ENV": f.provider,
			}
			for name, value := range runnerEnvironment {
				installEnvironment[name] = value
			}
			result := project.run(
				t,
				containerCall{checkout: f.directory, environment: installEnvironment},
				credentialArgs...,
			)
			if result.err != nil {
				t.Fatalf(
					"Credential installation failed; stdout=%q stderr=%q error=%v",
					result.stdout,
					result.stderr,
					result.err,
				)
			}
			for _, line := range strings.Split(strings.TrimSuffix(result.stdout, "\n"), "\n") {
				if !strings.HasPrefix(line, "::add-mask::") {
					t.Fatalf(
						"The credential command printed a line outside the masking protocol: %q.",
						line,
					)
				}
			}
			encoded := readFile(t, f.environment)
			values, _ := parseEnvironment(t, encoded)
			if len(values) != 3 || values["AWS_SECRET_ACCESS_KEY"] != backendValue ||
				values["PROVIDER_TOKEN"] != providerValue ||
				values["SSH_KEY"] != key {
				t.Fatal(
					"Environment exports did not preserve the separate bundles and key pathname",
				)
			}
			if strings.Contains(encoded, "synthetic-private-key-bytes") {
				t.Fatal("Environment exports contain private key bytes")
			}
			modes := project.run(
				t,
				containerCall{entrypoint: "stat"},
				"-c",
				"%a %F %n",
				credentialDirectory,
				keyDirectory,
				key,
			)
			expectedModes := "700 directory " + credentialDirectory + "\n700 directory " + keyDirectory + "\n600 regular file " + key + "\n"
			if modes.err != nil || modes.stdout != expectedModes {
				t.Fatalf(
					"The credential permission check failed or did not report %q: stdout=%q stderr=%q error=%v.",
					expectedModes,
					modes.stdout,
					modes.stderr,
					modes.err,
				)
			}
			contents := project.run(t, containerCall{entrypoint: "cat"}, key)
			if contents.err != nil || contents.stdout != keyContents {
				t.Fatalf(
					"Credential file bytes changed; stderr=%q error=%v",
					contents.stderr,
					contents.err,
				)
			}
			result = project.run(
				t,
				containerCall{checkout: f.directory, environment: runnerEnvironment},
				append(credentialArgs, "--cleanup")...,
			)
			if result.err != nil || result.stdout != "" {
				t.Fatalf(
					"Credential cleanup failed; stdout=%q stderr=%q error=%v",
					result.stdout,
					result.stderr,
					result.err,
				)
			}
			remaining := project.run(t, containerCall{entrypoint: "ls"}, "-A", containerRunner)
			if remaining.err != nil || remaining.stdout != "" {
				t.Fatalf(
					"Credential cleanup left runner files; stdout=%q stderr=%q error=%v",
					remaining.stdout,
					remaining.stderr,
					remaining.err,
				)
			}
		},
	)
}
