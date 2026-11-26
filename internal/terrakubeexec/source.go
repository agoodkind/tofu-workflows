package terrakubeexec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const mainRef = "refs/remotes/origin/main"

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func verifyCheckout(ctx context.Context, git commandRunner, origin, branch string) (string, error) {
	remote, err := gitOutput(ctx, git, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}

	if remote != origin {
		return "", errDifferentOrigin
	}

	commit, err := gitOutput(ctx, git, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", err
	}

	if !commitPattern.MatchString(commit) {
		return "", errInvalidCommit
	}

	shallow, err := gitOutput(ctx, git, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return "", err
	}

	fetch := []string{"fetch", "--no-tags"}
	if shallow == "true" {
		fetch = append(fetch, "--unshallow")
	}

	promotionRef := "refs/remotes/origin/" + branch
	fetch = append(
		fetch,
		origin,
		"+refs/heads/main:"+mainRef,
		"+refs/heads/"+branch+":"+promotionRef,
	)
	if _, err := gitOutput(ctx, git, fetch...); err != nil {
		return "", executionError("fetch trusted Configs main and promotion branch", err)
	}

	tip, err := gitOutput(ctx, git, "rev-parse", "--verify", promotionRef)
	if err != nil {
		return "", err
	}

	if commit != tip {
		return "", errPromotionChanged
	}

	history, err := gitOutput(ctx, git, "rev-list", "--first-parent", mainRef)
	if err != nil {
		return "", err
	}

	if !slices.Contains(strings.Fields(history), commit) {
		return "", errMainHistory
	}

	return commit, nil
}

func requireCleanCheckout(ctx context.Context, git commandRunner) error {
	status, err := gitOutput(ctx, git, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}

	if status != "" {
		return errDirtyCheckout
	}

	return nil
}

func excludeExecutorFiles(checkout string) error {
	directory := filepath.Join(checkout, ".git", "info")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return executionError("create Git info directory", err)
	}

	file, err := os.OpenFile(
		filepath.Join(directory, "exclude"),
		os.O_WRONLY|os.O_APPEND|os.O_CREATE,
		0o600,
	)
	if err != nil {
		return executionError("open Git local exclude file", err)
	}

	_, writeErr := file.WriteString("\n/commitHash.info\n/userScript.sh\n/.terrakube/\n")
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return executionError("record Terrakube local files", err)
	}

	return nil
}

var (
	errDifferentOrigin = errors.New(
		"configured Git origin differs from the trusted Configs source",
	)
	errInvalidCommit    = errors.New("invalid Configs commit from Git")
	errPromotionChanged = errors.New("checkout does not match the remote promotion branch tip")
	errMainHistory      = errors.New(
		"checkout commit is not on the first-parent history of Configs main",
	)
	errDirtyCheckout = errors.New("the Configs checkout has uncommitted changes")
)
