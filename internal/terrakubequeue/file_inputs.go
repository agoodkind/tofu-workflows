package terrakubequeue

import (
	"path"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

const pathTraversalLength = 2

// fileInputs also reads path expressions assigned to locals. A dynamic file
// name depends on the static directory before the interpolation.
func fileInputs(file *hcl.File, dir, root string) ([]string, error) {
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		// JSON configurations may contain expression strings anywhere in an object.
		// Treat the whole repository as an input until those expressions can be resolved.
		return []string{"."}, nil
	}

	var inputs []string
	diag := hclsyntax.VisitAll(
		body,
		func(node hclsyntax.Node) hcl.Diagnostics {
			switch expr := node.(type) {
			case *hclsyntax.TemplateExpr:
				if input, ok := templatePath(expr, dir, root); ok {
					inputs = append(inputs, input)
				}
			case *hclsyntax.FunctionCallExpr:
				if len(expr.Args) > 0 && fileFunction(expr.Name) {
					value, diag := expr.Args[0].Value(nil)
					if !diag.HasErrors() && value.Type().Equals(cty.String) && !value.IsNull() {
						inputs = append(inputs, path.Join(root, value.AsString()))
					}
				}
			}

			return nil
		},
	)

	if diag.HasErrors() {
		return nil, diag
	}

	return inputs, nil
}

func fileFunction(name string) bool {
	return slices.Contains(
		[]string{
			"file",
			"templatefile",
			"fileexists",
			"fileset",
			"filebase64",
			"filemd5",
			"filesha1",
			"filesha256",
			"filesha512",
			"filebase64sha256",
			"filebase64sha512",
		},
		name,
	)
}

func templatePath(expr *hclsyntax.TemplateExpr, dir, root string) (string, bool) {
	if len(expr.Parts) < pathTraversalLength {
		return "", false
	}

	traversal, diag := hcl.AbsTraversalForExpr(expr.Parts[0])
	if diag.HasErrors() || len(traversal) != pathTraversalLength || traversal.RootName() != "path" {
		return "", false
	}

	field, ok := traversal[1].(hcl.TraverseAttr)
	if !ok {
		return "", false
	}

	base, supported := map[string]string{"module": dir, "root": root, "cwd": root}[field.Name]
	if !supported {
		return "", false
	}

	var suffix strings.Builder

	for _, part := range expr.Parts[1:] {
		literal, ok := part.(*hclsyntax.LiteralValueExpr)
		if !ok || !literal.Val.Type().Equals(cty.String) {
			prefix := suffix.String()
			if !strings.HasSuffix(prefix, "/") {
				prefix = path.Dir(prefix)
			}

			return path.Join(base, prefix), true
		}

		suffix.WriteString(literal.Val.AsString())
	}

	return path.Join(base, suffix.String()), true
}
