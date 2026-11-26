package terrakubeexec

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	privateMode        = 0o600
	firstSignalablePID = 2
)

type jobPaths struct {
	root string
	home string
	tmp  string
}

func (p jobPaths) vaultPath() string {
	return filepath.Join(p.root, "vault.yml")
}

func prepareDirectories(root string) (jobPaths, error) {
	paths := jobPaths{
		root: root,
		home: filepath.Join(root, "home"),
		tmp:  filepath.Join(root, "tmp"),
	}
	for _, directory := range []string{
		paths.tmp,
		filepath.Join(paths.home, ".config", "ansible"),
		filepath.Join(paths.home, ".ssh"),
		filepath.Join(paths.home, ".cache"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return jobPaths{}, executionError("create protected job directory", err)
		}
	}

	return paths, nil
}

func isolatedEnvironment(paths jobPaths, origin, gitToken string) []string {
	authorization := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + gitToken))

	return []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + paths.home,
		"TMPDIR=" + paths.tmp,
		"XDG_CACHE_HOME=" + filepath.Join(paths.home, ".cache"),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=http." + origin + ".extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + authorization,
		"GIT_CONFIG_KEY_1=http.followRedirects",
		"GIT_CONFIG_VALUE_1=false",
	}
}

func prepareCredentials(paths jobPaths, getenv func(string) string) (string, error) {
	for _, credential := range []struct{ key, path string }{
		{key: "CONFIGS_DEPLOY_VAULT", path: paths.vaultPath()},
		{
			key:  "CONFIGS_DEPLOY_VAULT_PASSWORD",
			path: filepath.Join(paths.home, ".config", "ansible", "vault.pass"),
		},
		{
			key:  "CONFIGS_DEPLOY_SSH_KNOWN_HOSTS",
			path: filepath.Join(paths.home, ".ssh", "known_hosts"),
		},
	} {
		value := getenv(credential.key)
		if value == "" {
			slog.Error(
				"Missing deployment credential",
				"input",
				credential.key,
				"err",
				errMissingDeploymentInput,
			)

			return "", fmt.Errorf("%w: %s", errMissingDeploymentInput, credential.key)
		}

		if err := os.WriteFile(credential.path, []byte(value), privateMode); err != nil {
			return "", executionError("write protected "+credential.key+" file", err)
		}
	}

	token := getenv("CONFIGS_DEPLOY_PROVIDER_TOKEN")
	if token == "" {
		return "", errMissingProviderToken
	}

	return token, nil
}

func prepareSSH(
	ctx context.Context,
	dir string,
	environment []string,
	paths jobPaths,
	getenv func(string) string,
) (string, func(), error) {
	key := getenv("CONFIGS_DEPLOY_SSH_PRIVATE_KEY")
	if key == "" {
		return "", nil, errMissingSSHIdentity
	}

	socket := filepath.Join(paths.root, "agent.sock")
	var response strings.Builder
	start := runner(dir, environment, nil)
	if err := start(
		ctx,
		command{name: "/usr/bin/ssh-agent", args: []string{"-s", "-a", socket}, output: &response},
	); err != nil {
		return "", nil, executionError("start private SSH agent", err)
	}

	pid := agentPID(response.String())
	if pid < firstSignalablePID {
		return "", nil, errMissingSSHAgentPID
	}

	stop := func() {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	add := runner(dir, append(environment, "SSH_AUTH_SOCK="+socket), nil)
	if err := add(
		ctx,
		command{
			name:  "/usr/bin/ssh-add",
			args:  []string{"-"},
			input: strings.NewReader(key + "\n"),
		},
	); err != nil {
		stop()

		return "", nil, executionError("load workspace SSH identity", err)
	}

	return socket, stop, nil
}

func agentPID(response string) int {
	for line := range strings.SplitSeq(response, "\n") {
		value, ok := strings.CutPrefix(line, "SSH_AGENT_PID=")
		if ok {
			value, _, _ = strings.Cut(value, ";")
			pid, err := strconv.Atoi(value)
			if err == nil {
				return pid
			}
		}
	}

	return 0
}

var (
	errMissingDeploymentInput = errors.New("workspace is missing a required environment input")
	errMissingProviderToken   = errors.New("workspace requires CONFIGS_DEPLOY_PROVIDER_TOKEN")
	errMissingSSHIdentity     = errors.New("workspace requires CONFIGS_DEPLOY_SSH_PRIVATE_KEY")
	errMissingSSHAgentPID     = errors.New("SSH agent did not report its process ID")
)
