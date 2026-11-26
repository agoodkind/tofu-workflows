package terrakubequeue

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"
)

func (g gitSource) modulePaths(ctx context.Context, sha string) ([]string, error) {
	visited := map[string]bool{}

	pending := []string{g.Root}
	for len(pending) > 0 {
		dir := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if visited[dir] {
			continue
		}

		visited[dir] = true
		files, err := g.git(ctx, "ls-tree", "-z", "--name-only", sha+":"+dir)
		if err != nil {
			return nil, err
		}

		for file := range strings.SplitSeq(files, "\x00") {
			if !strings.HasSuffix(file, ".tf") && !strings.HasSuffix(file, ".tf.json") {
				continue
			}

			name := path.Join(dir, file)
			contents, err := g.git(ctx, "show", sha+":"+name)
			if err != nil {
				return nil, err
			}

			modules, err := moduleSources(name, []byte(contents+"\n"))
			if err != nil {
				return nil, err
			}

			for _, module := range modules {
				if !strings.HasPrefix(module, "./") && !strings.HasPrefix(module, "../") {
					continue
				}

				resolved := path.Join(dir, module)
				if strings.HasPrefix(resolved, "../") {
					slog.ErrorContext(
						ctx,
						"module resolves outside the repository",
						"module",
						module,
						"err",
						errQueue,
					)

					return nil, fmt.Errorf(
						"%w: module %s resolves outside Configs",
						errQueue,
						module,
					)
				}

				pending = append(pending, resolved)
			}
		}
	}

	dirs := make([]string, 0, len(visited))
	for dir := range visited {
		dirs = append(dirs, dir)
	}

	slices.Sort(dirs)

	return dirs, nil
}

func moduleSources(name string, contents []byte) ([]string, error) {
	parser := hclparse.NewParser()
	var file *hcl.File
	var diagnostics hcl.Diagnostics
	if strings.HasSuffix(name, ".json") {
		file, diagnostics = parser.ParseJSON(contents, name)
	} else {
		file, diagnostics = parser.ParseHCL(contents, name)
	}

	if diagnostics.HasErrors() {
		slog.Error("read module configuration", "file", name, "err", diagnostics)

		return nil, fmt.Errorf("parse %s: %w", name, diagnostics)
	}

	blocks, _, diagnostics := file.Body.PartialContent(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{{Type: "module", LabelNames: []string{"name"}}},
	})
	if diagnostics.HasErrors() {
		return nil, fmt.Errorf("read modules in %s: %w", name, diagnostics)
	}

	var sources []string

	for _, block := range blocks.Blocks {
		attrs, _, diag := block.Body.PartialContent(&hcl.BodySchema{
			Attributes: []hcl.AttributeSchema{{Name: "source", Required: true}},
		})
		if diag.HasErrors() {
			return nil, fmt.Errorf("read module source in %s: %w", name, diag)
		}

		value, diag := attrs.Attributes["source"].Expr.Value(nil)
		if diag.HasErrors() || value.IsNull() || !value.Type().Equals(cty.String) {
			return nil, fmt.Errorf(
				"%w: module source in %s must be a literal string",
				errQueue,
				name,
			)
		}

		sources = append(sources, value.AsString())
	}

	return sources, nil
}
