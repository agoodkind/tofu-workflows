package tack

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/agoodkind/tofu-workflows/internal/input"

	"github.com/agoodkind/tofu-workflows/internal/release"
)

var (
	errConfigPins                     = errors.New("invalid Configs release pins")
	errTackReleaseRequiresServerAudit = errors.New(
		"a Tack release requires the server and audit consumer from their published GHCR repositories",
	)
	errTackReleaseRequiresExactlyTwo = errors.New(
		"a Tack release requires exactly two overlay checksums",
	)
	errConfigsAlreadyRecordsProposedTack = errors.New(
		"the Configs main branch already records the proposed Tack commit under a different active release",
	)
	errConfigsLacksCurrentlySelectedTack = errors.New(
		"the Configs artifacts lack the currently selected Tack release",
	)
	errCurrentTackReleaseLacksAn = errors.New(
		"the current Tack release lacks an accountable email",
	)
	errCurrentTackReleaseSelectLiteral = errors.New(
		"the current Tack release must select a literal accountable email",
	)
	errExpectedTackReleaseMapClose = errors.New(
		"expected the Tack release map to close on its own line",
	)
)

const (
	imageCount         = 2
	overlayCount       = 2
	artifactsPath      = "opentofu/modules/configuration/artifacts/main.tf"
	qaPath             = "opentofu/modules/configuration/tack/qa/runtime.tf"
	prodPath           = "opentofu/modules/configuration/tack/prod/runtime.tf"
	serverRepository   = "ghcr.io/agoodkind/tack-server"
	consumerRepository = "ghcr.io/agoodkind/tack-audit-consumer"
	fdbOverlay         = "fdb-overlay/fdb.bash"
	yugabyteOverlay    = "yugabyte-overlay/yugabyted"
)

var tackPinPaths = [...]string{artifactsPath, qaPath, prodPath}

type tackRelease struct {
	source   string
	server   string
	consumer string
	fdb      string
	yugabyte string
}

func tackReferences(manifest release.Manifest) (tackRelease, error) {
	references := tackRelease{
		source:   strings.ToLower(manifest.SourceSHA),
		server:   strings.TrimPrefix(manifest.Artifacts["server"], serverRepository+"@"),
		consumer: strings.TrimPrefix(manifest.Artifacts["consumer"], consumerRepository+"@"),
	}
	if len(manifest.Artifacts) != imageCount || !input.Digest(references.server) ||
		!input.Digest(references.consumer) {
		return tackRelease{}, errTackReleaseRequiresServerAudit
	}

	if len(manifest.Variables) != overlayCount {
		return tackRelease{}, errTackReleaseRequiresExactlyTwo
	}

	for name, checksum := range map[string]*string{
		fdbOverlay:      &references.fdb,
		yugabyteOverlay: &references.yugabyte,
	} {
		value, err := strconv.Unquote(string(manifest.Variables[name]))
		if err != nil || !input.Digest("sha256:"+value) {
			slog.Error("Invalid Tack overlay checksum", "err", errConfigPins)

			return tackRelease{}, fmt.Errorf(
				"%w: release requires a SHA-256 checksum for %s",
				errConfigPins,
				name,
			)
		}

		*checksum = value
	}

	return references, nil
}

type fileEdit struct {
	rangeInFile hcl.Range
	replacement string
}

func editFile(text string, edits ...fileEdit) string {
	slices.SortFunc(
		edits,
		func(left, right fileEdit) int {
			return right.rangeInFile.Start.Byte - left.rangeInFile.Start.Byte
		},
	)

	for _, edit := range edits {
		text = text[:edit.rangeInFile.Start.Byte] + edit.replacement + text[edit.rangeInFile.End.Byte:]
	}

	return text
}

func findAttribute(
	text string,
	filename string,
	blockType string,
	label string,
	attribute string,
) (*hclsyntax.Attribute, error) {
	parsed, diagnostics := hclsyntax.ParseConfig([]byte(text), filename, hcl.InitialPos)
	if diagnostics.HasErrors() {
		slog.Error("Parse Configs pin file failed", "err", errConfigPins)

		return nil, fmt.Errorf("%w: parse %s: %s", errConfigPins, filename, diagnostics.Error())
	}

	body, ok := parsed.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("%w: expected HCL body in %s", errConfigPins, filename)
	}

	var expression *hclsyntax.Attribute

	for _, block := range body.Blocks {
		if block.Type != blockType || strings.Join(block.Labels, "") != label {
			continue
		}

		if attr := block.Body.Attributes[attribute]; attr != nil {
			if expression != nil {
				return nil, fmt.Errorf(
					"%w: %s declares %s more than once",
					errConfigPins,
					filename,
					attribute,
				)
			}

			expression = attr
		}
	}

	if expression == nil {
		return nil, fmt.Errorf("%w: %s must declare %s", errConfigPins, filename, attribute)
	}

	return expression, nil
}

func literalString(expression hclsyntax.Expression) (string, error) {
	value, diagnostics := expression.Value(nil)
	if diagnostics.HasErrors() || value.Type() != cty.String || value.IsNull() || !value.IsKnown() {
		slog.Error("Configs pin value requires a literal string", "err", errConfigPins)

		return "", fmt.Errorf(
			"%w: expected a literal string in %s",
			errConfigPins,
			expression.Range().String(),
		)
	}

	return value.AsString(), nil
}

func releasePin(text, filename string) (hcl.Range, string, error) {
	expression, err := findAttribute(text, filename, "locals", "", "release_commit")
	if err != nil {
		return hcl.Range{}, "", err
	}

	commit, err := literalString(expression.Expr)
	if err != nil || !input.Commit(commit) {
		slog.Error("Configs pin does not select an exact Tack commit", "err", errConfigPins)

		return hcl.Range{}, "", fmt.Errorf(
			"%w: %s must select an exact Tack commit",
			errConfigPins,
			filename,
		)
	}

	return expression.Expr.Range(), strings.ToLower(commit), nil
}

func outputObject(text, name string) (*hclsyntax.ObjectConsExpr, error) {
	expression, err := findAttribute(text, artifactsPath, "output", name, "value")
	if err != nil {
		return nil, err
	}

	object, ok := expression.Expr.(*hclsyntax.ObjectConsExpr)
	if !ok {
		slog.Error("Configs pin output requires a literal map", "err", errConfigPins)

		return nil, fmt.Errorf("%w: %s must be a literal map", errConfigPins, name)
	}

	return object, nil
}

func objectEntries(object *hclsyntax.ObjectConsExpr) (map[string]hclsyntax.Expression, error) {
	entries := make(map[string]hclsyntax.Expression, len(object.Items))
	for _, item := range object.Items {
		key, err := literalString(item.KeyExpr)
		if err != nil {
			return nil, err
		}

		if _, exists := entries[key]; exists {
			slog.Error("Configs pin map repeats a key", "err", errConfigPins)

			return nil, fmt.Errorf("%w: duplicate declaration for %q", errConfigPins, key)
		}

		entries[key] = item.ValueExpr
	}

	return entries, nil
}

func overlayEdits(text string, references tackRelease) ([]fileEdit, error) {
	object, err := outputObject(text, "tack_overlays")
	if err != nil {
		return nil, err
	}

	entries, err := objectEntries(object)
	if err != nil {
		return nil, err
	}

	edits := make([]fileEdit, 0, overlayCount)

	for name, checksum := range map[string]string{
		fdbOverlay:      references.fdb,
		yugabyteOverlay: references.yugabyte,
	} {
		expression := entries[name]
		if expression == nil {
			slog.Error("Configs pin file lacks an overlay", "err", errConfigPins)

			return nil, fmt.Errorf("%w: missing overlay %q", errConfigPins, name)
		}

		edits = append(edits, fileEdit{expression.Range(), strconv.Quote(checksum)})
	}

	return edits, nil
}

func releaseEntryEdit(text, current string, references tackRelease) (fileEdit, error) {
	object, err := outputObject(text, "tack_releases")
	if err != nil {
		return fileEdit{}, err
	}

	entries, err := objectEntries(object)
	if err != nil {
		return fileEdit{}, err
	}

	if _, exists := entries[references.source]; exists {
		return fileEdit{}, errConfigsAlreadyRecordsProposedTack
	}

	entry, ok := entries[current].(*hclsyntax.ObjectConsExpr)
	if !ok {
		return fileEdit{}, errConfigsLacksCurrentlySelectedTack
	}

	values, err := objectEntries(entry)
	if err != nil {
		return fileEdit{}, err
	}

	accountableExpression := values["accountable"]
	if accountableExpression == nil {
		return fileEdit{}, errCurrentTackReleaseLacksAn
	}

	accountable, err := literalString(accountableExpression)
	if err != nil || accountable == "" {
		return fileEdit{}, errCurrentTackReleaseSelectLiteral
	}

	end := object.Range().End.Byte
	line := strings.LastIndex(text[:end], "\n") + 1
	if line == 0 || strings.TrimSpace(text[line:end]) != "}" {
		return fileEdit{}, errExpectedTackReleaseMapClose
	}

	replacement := fmt.Sprintf(
		"    %q = {\n      commit          = %q\n      server_digest   = %q\n"+
			"      consumer_digest = %q\n      accountable     = %q\n    }\n",
		references.source,
		references.source,
		references.server,
		references.consumer,
		accountable,
	)

	return fileEdit{
		rangeInFile: hcl.Range{Start: hcl.Pos{Byte: line}, End: hcl.Pos{Byte: line}},
		replacement: replacement,
	}, nil
}

func updatedTackPins(
	original map[string]string,
	references tackRelease,
	current string,
) (map[string]string, error) {
	updated := make(map[string]string, len(tackPinPaths))

	for _, path := range []string{qaPath, prodPath} {
		expression, _, err := releasePin(original[path], path)
		if err != nil {
			return nil, err
		}

		updated[path] = editFile(
			original[path],
			fileEdit{expression, strconv.Quote(references.source)},
		)
	}

	text := original[artifactsPath]
	edits, err := overlayEdits(text, references)
	if err != nil {
		return nil, err
	}

	entry, err := releaseEntryEdit(text, current, references)
	if err != nil {
		return nil, err
	}

	updated[artifactsPath] = editFile(text, append(edits, entry)...)

	return updated, nil
}
