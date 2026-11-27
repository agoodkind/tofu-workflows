package terrakubequeue

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

const (
	gitMetadataLineEnding   = "\n"
	worktreeConfigExtension = "worktreeconfig"
	gitExtensionsSection    = "extensions"
	gitDirectoryPrefix      = "gitdir: "
	commonDirectoryFile     = "commondir"
)

// sharedObjectStorage lets the queue read commits and remote refs from linked
// checkouts. Git stores both in its common directory. go-git rejects
// worktreeConfig, so Config removes that option from its in-memory copy.
// For example, a linked checkout reads origin/main from that directory.
type sharedObjectStorage struct {
	*filesystem.Storage
}

func (s sharedObjectStorage) Config() (*config.Config, error) {
	cfg, err := s.Storage.Config()
	if err != nil {
		slog.Error("read shared Git configuration", "err", err)

		return nil, fmt.Errorf("read shared Git configuration: %w", err)
	}

	extension := cfg.Raw.Section(gitExtensionsSection)
	if !extension.HasOption(worktreeConfigExtension) {
		return nil, fmt.Errorf(
			"%w: expected extensions.worktreeConfig in common configuration",
			errQueue,
		)
	}

	extension.RemoveOption(worktreeConfigExtension)

	return cfg, nil
}

func openSharedObjects(checkout string) (*git.Repository, error) {
	dot := filepath.Join(checkout, git.GitDirName)
	info, err := os.Stat(dot)
	if err != nil {
		return nil, fmt.Errorf("locate %s: %w", dot, err)
	}

	if !info.IsDir() {
		data, err := os.ReadFile(dot)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", dot, err)
		}

		path, ok := strings.CutPrefix(
			strings.TrimSuffix(string(data), gitMetadataLineEnding),
			gitDirectoryPrefix,
		)
		if !ok || path == "" {
			return nil, fmt.Errorf("%w: invalid gitdir in %s", errQueue, dot)
		}

		dot = resolveGitPath(checkout, path)
	}

	gitDirectory, err := os.OpenRoot(dot)
	if err != nil {
		return nil, fmt.Errorf("open Git directory %s: %w", dot, err)
	}
	defer gitDirectory.Close()
	common, err := gitDirectory.ReadFile(commonDirectoryFile)
	if err == nil {
		dot = resolveGitPath(dot, strings.TrimSuffix(string(common), gitMetadataLineEnding))
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read commondir in %s: %w", dot, err)
	}

	storage := filesystem.NewStorage(osfs.New(dot), cache.NewObjectLRUDefault())
	repo, err := git.Open(sharedObjectStorage{storage}, nil)
	if err != nil {
		slog.Error("open shared Git objects", "directory", dot, "err", err)

		return nil, fmt.Errorf("open shared Git objects in %s: %w", dot, err)
	}

	return repo, nil
}

func resolveGitPath(base, path string) string {
	if filepath.IsAbs(path) {
		return path
	}

	return filepath.Join(base, path)
}
