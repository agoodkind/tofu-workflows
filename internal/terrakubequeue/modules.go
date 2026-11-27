package terrakubequeue

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"
)

const (
	hclConfigExtension    = ".tf"
	jsonConfigExtension   = ".tf.json"
	jsonExtension         = ".json"
	localModulePrefix     = "./"
	parentModulePrefix    = "../"
	moduleBlockType       = "module"
	moduleLabelName       = "name"
	moduleSourceAttribute = "source"
)

func (g gitSource) modulePaths(ctx context.Context, rootTree *object.Tree) ([]string, error) {
	visited := map[string]bool{}
	var inputs []string

	pending := []string{g.Root}
	for len(pending) > 0 {
		dir := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if visited[dir] {
			continue
		}

		visited[dir] = true
		if err := ctx.Err(); err != nil {
			slog.WarnContext(ctx, "read module cancelled", "directory", dir, "err", err)

			return nil, fmt.Errorf("read module %s: %w", dir, err)
		}

		tree := rootTree
		if dir != repoRootPath {
			var err error
			tree, err = rootTree.Tree(dir)
			if err != nil {
				return nil, fmt.Errorf("read module tree %s: %w", dir, err)
			}
		}

		for _, entry := range tree.Entries {
			file := entry.Name
			if !strings.HasSuffix(file, hclConfigExtension) &&
				!strings.HasSuffix(file, jsonConfigExtension) {
				continue
			}

			name := path.Join(dir, file)
			contents, err := trackedContents(rootTree, name)
			if err != nil {
				return nil, err
			}

			modules, dependencies, err := moduleSources(name, []byte(contents), dir, g.Root)
			if err != nil {
				return nil, err
			}

			inputs = append(inputs, dependencies...)

			localModules, err := resolveLocalModules(ctx, dir, modules)
			if err != nil {
				return nil, err
			}

			pending = append(pending, localModules...)
		}
	}

	dirs := make([]string, 0, len(visited))
	for dir := range visited {
		dirs = append(dirs, dir)
	}

	slices.Sort(dirs)

	return append(dirs, inputs...), nil
}

func resolveLocalModules(ctx context.Context, dir string, modules []string) ([]string, error) {
	var pending []string

	for _, module := range modules {
		if !strings.HasPrefix(module, localModulePrefix) &&
			!strings.HasPrefix(module, parentModulePrefix) {
			continue
		}

		resolved := path.Join(dir, module)
		if strings.HasPrefix(resolved, parentModulePrefix) {
			slog.ErrorContext(
				ctx,
				"module resolves outside the repository",
				"module",
				module,
				"err",
				errQueue,
			)

			return nil, fmt.Errorf("%w: module %s resolves outside Configs", errQueue, module)
		}

		pending = append(pending, resolved)
	}

	return pending, nil
}

func moduleSources(name string, contents []byte, dir, root string) ([]string, []string, error) {
	parser := hclparse.NewParser()
	var file *hcl.File
	var diagnostics hcl.Diagnostics
	if strings.HasSuffix(name, jsonExtension) {
		file, diagnostics = parser.ParseJSON(contents, name)
	} else {
		file, diagnostics = parser.ParseHCL(contents, name)
	}

	if diagnostics.HasErrors() {
		slog.Error("read module configuration", "file", name, "err", diagnostics)

		return nil, nil, fmt.Errorf("parse %s: %w", name, diagnostics)
	}

	blocks, _, diagnostics := file.Body.PartialContent(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: moduleBlockType, LabelNames: []string{moduleLabelName}},
		},
	})
	if diagnostics.HasErrors() {
		return nil, nil, fmt.Errorf("read modules in %s: %w", name, diagnostics)
	}

	var sources []string

	for _, block := range blocks.Blocks {
		attrs, _, diag := block.Body.PartialContent(&hcl.BodySchema{
			Attributes: []hcl.AttributeSchema{{Name: moduleSourceAttribute, Required: true}},
		})
		if diag.HasErrors() {
			return nil, nil, fmt.Errorf("read module source in %s: %w", name, diag)
		}

		value, diag := attrs.Attributes[moduleSourceAttribute].Expr.Value(nil)
		if diag.HasErrors() || value.IsNull() || !value.Type().Equals(cty.String) {
			return nil, nil, fmt.Errorf(
				"%w: module source in %s must be a literal string",
				errQueue,
				name,
			)
		}

		sources = append(sources, value.AsString())
	}

	inputs, err := fileInputs(file, dir, root)

	return sources, inputs, err
}
