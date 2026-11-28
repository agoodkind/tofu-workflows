package terrakubepreview

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	selectorName     = "terrakube-queue"
	selectorRevision = "43d2caf35060bea2c9510e79ee9c0ed2df31d721"
)

var (
	errSelector = errors.New("install the pinned terrakube-queue binary from a clean checkout")
	errDecision = errors.New("the root selector returned an invalid decision")
)

func selectRoot(ctx context.Context, root, base, head string) (bool, error) {
	executable, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("locate preview executable: %w", err)
	}

	selectorBinary := filepath.Join(filepath.Dir(executable), selectorName)
	info, err := buildinfo.ReadFile(selectorBinary)
	if err != nil {
		slog.Error("Could not read root selector build information", "err", err)

		return false, fmt.Errorf("open root selector: %w", err)
	}

	settings := make(map[string]string)

	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}

	if info.Path != "github.com/agoodkind/tofu-workflows/cmd/terrakube-queue" ||
		settings["vcs.revision"] != selectorRevision ||
		settings["vcs.modified"] != "false" {
		return false, errSelector
	}

	// The caller validates both 40-digit commit IDs. Root paths come from the fixed list below.
	// #nosec G204 G702 -- The command uses a pinned binary and passes literal arguments without a shell.
	cmd := exec.CommandContext(ctx, selectorBinary, "affected", ".", root, base, head)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "GIT_NO_REPLACE_OBJECTS=1"}
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("select affected workspace %s: %w", root, err)
	}

	value := strings.TrimSpace(string(out))
	if value != "true" && value != "false" {
		return false, errDecision
	}

	return value == "true", nil
}

func affectedWorkspaces(
	ctx context.Context,
	base string,
	head string,
	selectAffected func(context.Context, string, string, string) (bool, error),
) ([]string, error) {
	var selected []string

	for _, root := range []struct{ path, workspace string }{
		{"opentofu/suburban/tack-qa", os.Getenv("TERRAKUBE_PREVIEW_QA_ID")},
		{"opentofu/vault/tack-prod", os.Getenv("TERRAKUBE_PREVIEW_PROD_ID")},
	} {
		if !uuid.MatchString(root.workspace) {
			return nil, errWorkspace
		}

		affected, err := selectAffected(ctx, root.path, base, head)
		if err != nil {
			return nil, err
		}

		if affected {
			selected = append(selected, root.workspace)
		}
	}

	return selected, nil
}
