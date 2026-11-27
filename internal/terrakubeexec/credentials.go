package terrakubeexec

import (
	"context"
	"encoding/base64"
	"errors"
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

func prepareDirectories(root string) (jobPaths, error) {
	paths := jobPaths{
		root: root,
		home: filepath.Join(root, "home"),
		tmp:  filepath.Join(root, "tmp"),
	}
	for _, directory := range []string{
		paths.tmp,
		filepath.Join(paths.home, ".ssh"),
		filepath.Join(paths.home, ".cache"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return jobPaths{}, executionError("create protected job directory", err)
		}
	}

	return paths, nil
}

func isolatedEnvironment(paths jobPaths) []string {
	return []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + paths.home,
		"TMPDIR=" + paths.tmp,
		"XDG_CACHE_HOME=" + filepath.Join(paths.home, ".cache"),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	}
}

func gitCredentials(origin, gitToken string) []string {
	authorization := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + gitToken))

	return []string{
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=http." + origin + ".extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + authorization,
		"GIT_CONFIG_KEY_1=http.followRedirects",
		"GIT_CONFIG_VALUE_1=false",
	}
}

func prepareSSH(
	ctx context.Context,
	dir string,
	environment []string,
	paths jobPaths,
	key string,
	knownHosts string,
) (string, func(), error) {
	if err := os.WriteFile(filepath.Join(paths.home, ".ssh", "known_hosts"), []byte(knownHosts), privateMode); err != nil {
		return "", nil, executionError("write protected SSH known hosts file", err)
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

var errMissingSSHAgentPID = errors.New("SSH agent did not report its process ID")
