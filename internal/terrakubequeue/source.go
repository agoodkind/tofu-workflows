package terrakubequeue

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

var revision = regexp.MustCompile("^[0-9a-f]{40}$")

// gitSource reads main and the promotion refs in the Configs checkout.
type gitSource struct {
	Directory string
	Branch    string
	Root      string
}

func (g gitSource) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.Directory
	data, err := cmd.CombinedOutput()
	if err != nil {
		slog.ErrorContext(ctx, "Git command failed", "operation", args[0], "error", err)

		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(data)))
	}

	return strings.TrimSpace(string(data)), nil
}

func (g gitSource) promotion(ctx context.Context) (string, error) {
	return g.git(ctx, "rev-parse", "--verify", "refs/remotes/origin/"+g.Branch+"^{commit}")
}

func (g gitSource) nextCommit(ctx context.Context, current string) (string, error) {
	if !revision.MatchString(current) {
		return "", errCursor
	}

	history, err := g.git(ctx, "rev-list", "--first-parent", "refs/remotes/origin/main")
	if err != nil {
		return "", err
	}

	commits := strings.Fields(history)
	index := slices.Index(commits, current)
	if index < 0 {
		slog.ErrorContext(
			ctx,
			"promotion commit is outside main",
			"commit",
			current,
			"err",
			errQueue,
		)

		return "", fmt.Errorf(
			"%w: promotion %s is outside Configs main first-parent history",
			errQueue,
			current,
		)
	}

	for indexFromTip := index - 1; indexFromTip >= 0; indexFromTip-- {
		commit := commits[indexFromTip]
		affected, err := Affected(ctx, g.Directory, g.Root, commits[indexFromTip+1], commit)
		if err != nil {
			return "", err
		}

		if affected {
			return commit, nil
		}
	}

	return "", nil
}

func (g gitSource) promote(ctx context.Context, current, next string) error {
	if !revision.MatchString(current) || !revision.MatchString(next) {
		return errRevision
	}

	// Check the server's advertised update result. Git can report success without
	// updating a ref when a competing worker already pushed the same commit.
	ref := "refs/heads/" + g.Branch
	result, err := g.git(
		ctx,
		"push",
		"--porcelain",
		"--force-with-lease="+ref+":"+current,
		"origin",
		next+":"+ref,
	)
	if err != nil {
		return err
	}

	for line := range strings.SplitSeq(result, "\n") {
		if strings.HasPrefix(line, " \t"+next+":"+ref+"\t") {
			return nil
		}
	}

	slog.ErrorContext(
		ctx,
		"promotion ref did not advance",
		"ref",
		ref,
		"commit",
		next,
		"err",
		errQueue,
	)

	return fmt.Errorf("%w: git did not confirm an exclusive fast-forward of %s", errQueue, ref)
}

func affects(files, dirs []string) bool {
	for _, file := range files {
		for _, dir := range dirs {
			if file == dir || strings.HasPrefix(file, dir+"/") {
				return true
			}
		}
	}

	return false
}

var (
	errCursor   = errors.New("missing full cursor SHA")
	errRevision = errors.New("promotion requires full commit IDs")
)

// Affected checks a revision for changes to workspace inputs.
//
// It compares the supplied Git trees and reads local module declarations from
// the head tree without executing code from either revision.
//
// Example:
//
//	changed, err := Affected(ctx, checkout, root, mergeBase, pullRequestHead)
func Affected(ctx context.Context, checkout, root, base, head string) (bool, error) {
	base = canonicalCommit(base)
	head = canonicalCommit(head)
	if base == "" || head == "" {
		return false, errRevision
	}

	source := gitSource{Directory: checkout, Root: root}
	files, err := source.git(ctx, "diff", "--no-renames", "--name-only", "-z", base, head, "--")
	if err != nil {
		return false, err
	}

	changed := strings.Split(files, "\x00")
	paths := []string{
		root,
		"configsctl",
		"configsctl.yml",
		"common",
		"ansible/inventory/group_vars/all",
		"opentofu/guest",
		"opentofu/shared",
		"opentofu/terraform.d",
		"opentofu/terraform.tfvars",
	}
	if affects(changed, paths) {
		return true, nil
	}

	if !affects(changed, []string{"opentofu/modules"}) {
		return false, nil
	}

	modules, err := source.modulePaths(ctx, head)
	if err != nil {
		return false, err
	}

	return affects(changed, modules), nil
}

func canonicalCommit(value string) string {
	const shaBytes = 20
	var digest [shaBytes]byte
	if len(value) != hex.EncodedLen(len(digest)) {
		return ""
	}

	if _, err := hex.Decode(digest[:], []byte(value)); err != nil {
		return ""
	}

	return hex.EncodeToString(digest[:])
}
