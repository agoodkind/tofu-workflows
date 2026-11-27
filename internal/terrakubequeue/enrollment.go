package terrakubequeue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"strings"

	strictjson "sigs.k8s.io/json"
)

const registryPath = ".github/terrakube/enrollments.json"

var (
	slug        = regexp.MustCompile("^[a-z0-9]+(-[a-z0-9]+)*$")
	repoName    = regexp.MustCompile("^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$")
	repoPath    = regexp.MustCompile("^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$")
	errRegistry = errors.New("invalid Terrakube enrollment")
	errDisabled = errors.New("deployment is disabled or its protected environment changed")
)

type registry struct {
	Version int           `json:"version"`
	Apps    []application `json:"apps"`
}
type application struct {
	Name        string          `json:"name"`
	Repository  string          `json:"repository"`
	Deployments []target        `json:"deployments"`
	Release     *releaseAdapter `json:"release,omitempty"`
}
type releaseAdapter struct {
	Adapter      string `json:"adapter"`
	SourceBranch string `json:"source_branch"`
	Workflow     string `json:"workflow"`
}
type target struct {
	Name        string   `json:"name"`
	Root        string   `json:"root"`
	Branch      string   `json:"promotion_branch"`
	Environment string   `json:"environment"`
	Enabled     *bool    `json:"enabled"`
	Shared      []string `json:"shared_inputs"`
}

func validRepoPath(value string) bool {
	return repoPath.MatchString(value) && path.Clean(value) == value && value != "." &&
		!slices.Contains(strings.Split(strings.ToLower(value), "/"), "..") &&
		!slices.Contains(strings.Split(strings.ToLower(value), "/"), ".git")
}

func (entry target) valid() bool {
	if !slug.MatchString(entry.Name) || !slug.MatchString(entry.Environment) ||
		entry.Enabled == nil ||
		entry.Shared == nil {
		return false
	}

	if !validRepoPath(entry.Root) || !strings.HasPrefix(entry.Root, "opentofu/") {
		return false
	}

	branch, ok := strings.CutPrefix(entry.Branch, "terrakube/")
	if !ok || !slug.MatchString(branch) {
		return false
	}

	shared := map[string]bool{}

	for _, input := range entry.Shared {
		if !validRepoPath(input) || shared[input] {
			return false
		}

		shared[input] = true
	}

	return true
}

func (catalog registry) targets() ([]target, error) {
	if catalog.Version != 1 || catalog.Apps == nil {
		return nil, errRegistry
	}

	used := map[string]bool{}
	var roots []string
	result := []target{}

	for _, app := range catalog.Apps {
		repo := strings.ToLower(app.Repository)
		if !slug.MatchString(app.Name) || !repoName.MatchString(repo) || used["app:"+app.Name] ||
			used["repo:"+repo] ||
			len(app.Deployments) == 0 {
			return nil, errRegistry
		}

		if app.Release != nil && !app.Release.valid() {
			return nil, errRegistry
		}

		used["app:"+app.Name] = true
		used["repo:"+repo] = true

		for _, entry := range app.Deployments {
			if !entry.valid() || used["name:"+entry.Name] || used["branch:"+entry.Branch] ||
				used["env:"+entry.Environment] {
				return nil, errRegistry
			}

			for _, root := range roots {
				if overlaps(entry.Root, root) {
					return nil, errRegistry
				}
			}

			used["name:"+entry.Name] = true
			used["branch:"+entry.Branch] = true
			used["env:"+entry.Environment] = true
			roots = append(roots, entry.Root)
			result = append(result, entry)
		}
	}

	if !independentRoots(result, roots) {
		return nil, errRegistry
	}

	return result, nil
}

func (release releaseAdapter) valid() bool {
	return slug.MatchString(release.Adapter) && validRepoPath(release.SourceBranch) &&
		validRepoPath(release.Workflow) &&
		path.Dir(release.Workflow) == ".github/workflows" &&
		slices.Contains([]string{".yml", ".yaml"}, path.Ext(release.Workflow))
}

func independentRoots(entries []target, roots []string) bool {
	for _, entry := range entries {
		for _, input := range entry.Shared {
			for _, root := range roots {
				if root != entry.Root && overlaps(input, root) {
					return false
				}
			}
		}
	}

	return true
}

func overlaps(left, right string) bool {
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

func readTargets(ctx context.Context, checkout string) ([]target, error) {
	source := gitSource{Directory: checkout}
	repo, err := source.repository()
	if err != nil {
		return nil, err
	}

	commit, err := commitAtRef(repo, originMain)
	if err != nil {
		return nil, err
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("read tree at %s: %w", commit.Hash, err)
	}

	data, err := trackedContents(tree, registryPath)
	if err != nil {
		return nil, err
	}

	var catalog registry
	issues, decodeErr := strictjson.UnmarshalStrict([]byte(data), &catalog)
	if err := errors.Join(append(issues, decodeErr)...); err != nil {
		slog.ErrorContext(ctx, "read Configs enrollments", "err", err)

		return nil, fmt.Errorf("decode reviewed enrollments: %w", err)
	}

	targets, err := catalog.targets()
	if err != nil {
		return nil, err
	}

	for _, entry := range targets {
		_, err := tree.Tree(entry.Root)
		if err != nil {
			return nil, fmt.Errorf("read enrolled root %s at %s: %w", entry.Root, commit.Hash, err)
		}
	}

	return targets, nil
}
