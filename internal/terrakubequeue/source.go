package terrakubequeue

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

const (
	shaBytes      = 20
	originMain    = "refs/remotes/origin/main"
	configsOrigin = "https://github.com/agoodkind/configs"
)

var revision = regexp.MustCompile("^[0-9a-f]{40}$")

// gitSource reads main and the promotion refs in the Configs checkout.
type gitSource struct {
	Directory        string
	Branch           string
	Root             string
	Shared           []string
	allowLocalRemote bool
}

func (g gitSource) repository() (*git.Repository, error) {
	return g.openCheckout(true)
}

func (g gitSource) openCheckout(sharedObjectsOnly bool) (*git.Repository, error) {
	repo, err := git.PlainOpenWithOptions(
		g.Directory,
		&git.PlainOpenOptions{EnableDotGitCommonDir: true},
	)
	if sharedObjectsOnly &&
		(errors.Is(
			err,
			git.ErrUnsupportedExtensionRepositoryFormatVersion,
		) || errors.Is(err, git.ErrUnknownExtension)) {
		return openSharedObjects(g.Directory)
	}

	if err != nil {
		slog.Error("open Git checkout", "directory", g.Directory, "err", err)

		return nil, fmt.Errorf("open Git checkout %s: %w", g.Directory, err)
	}

	return repo, nil
}

func commitAtRef(repo *git.Repository, name plumbing.ReferenceName) (*object.Commit, error) {
	ref, err := repo.Reference(name, true)
	if err != nil {
		slog.Error("resolve Git ref", "ref", name, "err", err)

		return nil, fmt.Errorf("resolve Git ref %s: %w", name, err)
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("read commit for %s: %w", name, err)
	}

	return commit, nil
}

func (g gitSource) promotion(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		slog.WarnContext(ctx, "read promotion cancelled", "err", err)

		return "", fmt.Errorf("read promotion: %w", err)
	}

	repo, err := g.repository()
	if err != nil {
		return "", err
	}

	commit, err := commitAtRef(repo, plumbing.NewRemoteReferenceName("origin", g.Branch))
	if err != nil {
		return "", err
	}

	return commit.Hash.String(), nil
}

func (g gitSource) nextCommit(ctx context.Context, current string) (string, error) {
	if !revision.MatchString(current) {
		return "", errCursor
	}

	repo, err := g.repository()
	if err != nil {
		return "", err
	}

	tip, err := commitAtRef(repo, originMain)
	if err != nil {
		return "", err
	}

	var commits []string

	for tip.Hash.String() != current {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("read main history: %w", err)
		}

		if tip.NumParents() == 0 {
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

		commits = append(commits, tip.Hash.String())
		parent, err := tip.Parent(0)
		if err != nil {
			return "", fmt.Errorf("read first parent of %s: %w", tip.Hash, err)
		}

		tip = parent
	}

	slices.Reverse(commits)

	for _, commit := range commits {
		affected, err := g.affected(ctx, current, commit)
		current = commit
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

	repo, err := g.openCheckout(false)
	if err != nil {
		return err
	}

	remote, err := repo.Remote("origin")
	if err != nil {
		return fmt.Errorf("read origin in %s: %w", g.Directory, err)
	}

	urls := remote.Config().URLs
	if len(urls) != 1 {
		return fmt.Errorf("%w: expected a single Configs origin", errQueue)
	}

	ref := plumbing.NewBranchReferenceName(g.Branch)
	if err := ref.Validate(); err != nil {
		return fmt.Errorf("validate promotion ref %s: %w", ref, err)
	}

	options := &git.PushOptions{
		RemoteName:        "origin",
		RemoteURL:         urls[0],
		RefSpecs:          []config.RefSpec{config.RefSpec(next + ":" + ref.String())},
		RequireRemoteRefs: []config.RefSpec{config.RefSpec(current + ":" + ref.String())},
	}
	if !g.allowLocalRemote || !filepath.IsAbs(urls[0]) {
		if urls[0] != configsOrigin && urls[0] != configsOrigin+".git" {
			return fmt.Errorf("%w: unexpected Configs origin", errQueue)
		}

		token := os.Getenv("PROMOTION_GITHUB_TOKEN")
		if token == "" {
			return fmt.Errorf("%w: PROMOTION_GITHUB_TOKEN is required", errQueue)
		}

		options.Auth = &githttp.BasicAuth{Username: "x-access-token", Password: token}
	}

	// RequireRemoteRefs checks the advertised commit. Receive-pack also compares
	// this commit under the server's ref lock before it accepts the update.
	if err := remote.PushContext(ctx, options); err != nil {
		slog.ErrorContext(ctx, "claim promotion ref", "ref", ref, "err", err)

		return fmt.Errorf("claim promotion %s: %w", ref, err)
	}

	return nil
}

func affects(files, dirs []string) bool {
	for _, file := range files {
		for _, dir := range dirs {
			if file != "" && (dir == "." || file == dir || strings.HasPrefix(file, dir+"/")) {
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

func (source gitSource) affected(ctx context.Context, base, head string) (bool, error) {
	base = canonicalCommit(base)
	head = canonicalCommit(head)
	if base == "" || head == "" {
		return false, errRevision
	}

	repo, err := source.repository()
	if err != nil {
		return false, err
	}

	before, err := commitTree(repo, base)
	if err != nil {
		return false, err
	}

	after, err := commitTree(repo, head)
	if err != nil {
		return false, err
	}

	changes, err := object.DiffTreeContext(ctx, before, after)
	if err != nil {
		slog.ErrorContext(ctx, "compare commit trees", "base", base, "head", head, "err", err)

		return false, fmt.Errorf("compare commits %s and %s: %w", base, head, err)
	}

	var changed []string

	for _, change := range changes {
		changed = append(changed, change.From.Name, change.To.Name)
	}

	paths := append(slices.Clone(source.Shared), source.Root)
	if affects(changed, paths) {
		return true, nil
	}

	if len(changes) == 0 {
		return false, nil
	}

	for _, tree := range []*object.Tree{before, after} {
		modules, err := source.modulePaths(ctx, tree)
		if err != nil {
			return false, err
		}

		if affects(changed, modules) {
			return true, nil
		}
	}

	return false, nil
}

func commitTree(repo *git.Repository, sha string) (*object.Tree, error) {
	commit, err := repo.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		slog.Error("read Git commit", "commit", sha, "err", err)

		return nil, fmt.Errorf("read commit %s: %w", sha, err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("read tree at %s: %w", sha, err)
	}

	return tree, nil
}

func trackedContents(tree *object.Tree, path string) (string, error) {
	file, err := tree.File(path)
	if err != nil {
		slog.Error("open tracked file", "path", path, "tree", tree.Hash, "err", err)

		return "", fmt.Errorf("open %s in tree %s: %w", path, tree.Hash, err)
	}

	data, err := file.Contents()
	if err != nil {
		return "", fmt.Errorf("read %s in tree %s: %w", path, tree.Hash, err)
	}

	return data, nil
}

func canonicalCommit(value string) string {
	var digest [shaBytes]byte
	if len(value) != hex.EncodedLen(len(digest)) {
		return ""
	}

	if _, err := hex.Decode(digest[:], []byte(value)); err != nil {
		return ""
	}

	return hex.EncodeToString(digest[:])
}
