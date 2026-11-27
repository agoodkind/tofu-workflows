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

	git "github.com/go-git/go-git/v5"
	strictjson "sigs.k8s.io/json"
)

const (
	registryVersion        = 1
	registryPath           = ".github/terrakube/enrollments.json"
	appSlugPattern         = "^[a-z0-9]+(-[a-z0-9]+)*$"
	repositoryNamePattern  = "^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$"
	repositoryPathPattern  = "^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$"
	repoRootPath           = "."
	repoPathSeparator      = "/"
	parentPathComponent    = ".."
	openTofuRootPrefix     = "opentofu/"
	promotionBranchPrefix  = "terrakube/"
	appIndexPrefix         = "app:"
	repositoryIndexPrefix  = "repo:"
	targetNameIndexPrefix  = "name:"
	branchIndexPrefix      = "branch:"
	environmentIndexPrefix = "env:"
	workflowsDirectory     = ".github/workflows"
	ymlExtension           = ".yml"
	yamlExtension          = ".yaml"
)

var (
	slug        = regexp.MustCompile(appSlugPattern)
	repoName    = regexp.MustCompile(repositoryNamePattern)
	repoPath    = regexp.MustCompile(repositoryPathPattern)
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
	components := strings.Split(strings.ToLower(value), repoPathSeparator)

	return repoPath.MatchString(value) && path.Clean(value) == value && value != repoRootPath &&
		!slices.Contains(
			components,
			parentPathComponent,
		) && !slices.Contains(components, git.GitDirName)
}

func (entry target) valid() bool {
	if !slug.MatchString(entry.Name) || !slug.MatchString(entry.Environment) ||
		entry.Enabled == nil ||
		entry.Shared == nil {
		return false
	}

	if !validRepoPath(entry.Root) || !strings.HasPrefix(entry.Root, openTofuRootPrefix) {
		return false
	}

	branch, ok := strings.CutPrefix(entry.Branch, promotionBranchPrefix)
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
	if catalog.Version != registryVersion || catalog.Apps == nil {
		return nil, errRegistry
	}

	used := map[string]bool{}
	result := []target{}

	for _, app := range catalog.Apps {
		repo := strings.ToLower(app.Repository)
		if !slug.MatchString(app.Name) || !repoName.MatchString(repo) ||
			used[appIndexPrefix+app.Name] ||
			used[repositoryIndexPrefix+repo] ||
			len(app.Deployments) == 0 {
			return nil, errRegistry
		}

		if app.Release != nil && !app.Release.valid() {
			return nil, errRegistry
		}

		used[appIndexPrefix+app.Name] = true
		used[repositoryIndexPrefix+repo] = true

		for _, entry := range app.Deployments {
			if !entry.valid() || used[targetNameIndexPrefix+entry.Name] ||
				used[branchIndexPrefix+entry.Branch] ||
				used[environmentIndexPrefix+entry.Environment] {
				return nil, errRegistry
			}

			for _, existing := range result {
				if overlaps(entry.Root, existing.Root) {
					return nil, errRegistry
				}
			}

			used[targetNameIndexPrefix+entry.Name] = true
			used[branchIndexPrefix+entry.Branch] = true
			used[environmentIndexPrefix+entry.Environment] = true
			result = append(result, entry)
		}
	}

	if !independentRoots(result) {
		return nil, errRegistry
	}

	return result, nil
}

func (release releaseAdapter) valid() bool {
	return slug.MatchString(release.Adapter) && validRepoPath(release.SourceBranch) &&
		validRepoPath(release.Workflow) &&
		path.Dir(release.Workflow) == workflowsDirectory &&
		slices.Contains([]string{ymlExtension, yamlExtension}, path.Ext(release.Workflow))
}

func independentRoots(entries []target) bool {
	for _, entry := range entries {
		for _, input := range entry.Shared {
			for _, other := range entries {
				if other.Root != entry.Root && overlaps(input, other.Root) {
					return false
				}
			}
		}
	}

	return true
}

func overlaps(left, right string) bool {
	return left == right || strings.HasPrefix(left, right+repoPathSeparator) ||
		strings.HasPrefix(right, left+repoPathSeparator)
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
		slog.ErrorContext(ctx, "read deployment enrollments", "err", err)

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
