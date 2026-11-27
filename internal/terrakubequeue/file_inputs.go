package terrakubequeue

import (
	"path"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

const (
	pathTraversalLength          = 2
	fileFunctionName             = "file"
	templateFileFunctionName     = "templatefile"
	fileExistsFunctionName       = "fileexists"
	fileSetFunctionName          = "fileset"
	fileBase64FunctionName       = "filebase64"
	fileMD5FunctionName          = "filemd5"
	fileSHA1FunctionName         = "filesha1"
	fileSHA256FunctionName       = "filesha256"
	fileSHA512FunctionName       = "filesha512"
	fileBase64SHA256FunctionName = "filebase64sha256"
	fileBase64SHA512FunctionName = "filebase64sha512"
	pathObjectName               = "path"
	pathModuleField              = "module"
	pathRootField                = "root"
	pathWorkingDirectoryField    = "cwd"
)

// fileInputs also reads path expressions assigned to locals. A dynamic file
// name depends on the static directory before the interpolation.
func fileInputs(file *hcl.File, dir, root string) []string {
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		// JSON configurations may contain expression strings anywhere in an object.
		// Treat the whole repository as an input until those expressions can be resolved.
		return []string{repoRootPath}
	}

	var inputs []string
	_ = hclsyntax.VisitAll(
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

	return inputs
}

func fileFunction(name string) bool {
	return slices.Contains(
		[]string{
			fileFunctionName,
			templateFileFunctionName,
			fileExistsFunctionName,
			fileSetFunctionName,
			fileBase64FunctionName,
			fileMD5FunctionName,
			fileSHA1FunctionName,
			fileSHA256FunctionName,
			fileSHA512FunctionName,
			fileBase64SHA256FunctionName,
			fileBase64SHA512FunctionName,
		},
		name,
	)
}

func templatePath(expr *hclsyntax.TemplateExpr, dir, root string) (string, bool) {
	if len(expr.Parts) < pathTraversalLength {
		return "", false
	}

	traversal, diag := hcl.AbsTraversalForExpr(expr.Parts[0])
	if diag.HasErrors() || len(traversal) != pathTraversalLength ||
		traversal.RootName() != pathObjectName {
		return "", false
	}

	field, ok := traversal[1].(hcl.TraverseAttr)
	if !ok {
		return "", false
	}

	var base string
	switch field.Name {
	case pathModuleField:
		base = dir
	case pathRootField, pathWorkingDirectoryField:
		base = root
	default:
		return "", false
	}

	var suffix strings.Builder

	for _, part := range expr.Parts[1:] {
		literal, ok := part.(*hclsyntax.LiteralValueExpr)
		if !ok || !literal.Val.Type().Equals(cty.String) {
			prefix := suffix.String()
			if !strings.HasSuffix(prefix, repoPathSeparator) {
				prefix = path.Dir(prefix)
			}

			return path.Join(base, prefix), true
		}

		suffix.WriteString(literal.Val.AsString())
	}

	return path.Join(base, suffix.String()), true
}
