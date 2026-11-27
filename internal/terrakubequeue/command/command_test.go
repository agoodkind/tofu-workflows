package command

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/agoodkind/tofu-workflows/internal/terrakubequeue"
)

const (
	testCheckout          = "configs"
	testHelpDeployment    = "help"
	testHelpFlag          = "--help"
	testInvalidMode       = "invalid"
	testWorkspace         = "opentofu/qa"
	testBase              = "base"
	testHead              = "head"
	testFalseOutput       = "false\n"
	testEmptyMatrixOutput = "[]\n"
)

type recordingOperations struct {
	deployArgs     []string
	matrixCheckout string
	matrixPreview  bool
	affectedArgs   []string
}

func (operations *recordingOperations) Deploy(
	_ context.Context,
	deployment string,
	checkout string,
) error {
	operations.deployArgs = []string{deployment, checkout}

	return nil
}

func (operations *recordingOperations) Matrix(
	_ context.Context,
	checkout string,
	includeDisabled bool,
) ([]terrakubequeue.Target, error) {
	operations.matrixCheckout = checkout
	operations.matrixPreview = includeDisabled

	return nil, nil
}

func (operations *recordingOperations) Affected(
	_ context.Context,
	checkout string,
	root string,
	base string,
	head string,
) (bool, error) {
	operations.affectedArgs = []string{checkout, root, base, head}

	return false, nil
}

func execute(t *testing.T, operations Operations, args ...string) (string, error) {
	t.Helper()

	cmd := New(operations)

	var output bytes.Buffer

	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs(args)

	err := cmd.ExecuteContext(t.Context())

	return output.String(), err
}

func TestQueueDeploymentUsesPositionalName(t *testing.T) {
	for _, name := range []string{matrixDeployMode, testHelpDeployment} {
		t.Run(
			name,
			func(t *testing.T) {
				operations := &recordingOperations{}
				args := []string{name, testCheckout}

				output, err := execute(t, operations, args...)
				if err != nil || output != "" || !reflect.DeepEqual(operations.deployArgs, args) {
					t.Fatalf(
						"deploy called with %v, output %q, error %v",
						operations.deployArgs,
						output,
						err,
					)
				}
			},
		)
	}
}

func TestQueueMatrixSelectsModeAndWritesAnEmptyArray(t *testing.T) {
	for _, mode := range []string{matrixDeployMode, matrixPreviewMode} {
		t.Run(
			mode,
			func(t *testing.T) {
				operations := &recordingOperations{}

				output, err := execute(t, operations, matrixCommandName, testCheckout, mode)
				if err != nil || output != testEmptyMatrixOutput {
					t.Fatalf("matrix output %q, error %v", output, err)
				}

				if operations.matrixCheckout != testCheckout ||
					operations.matrixPreview != (mode == matrixPreviewMode) {
					t.Fatalf(
						"matrix called with %q, preview %v",
						operations.matrixCheckout,
						operations.matrixPreview,
					)
				}
			},
		)
	}
}

func TestQueueAffectedWritesTheResult(t *testing.T) {
	operations := &recordingOperations{}
	args := []string{testCheckout, testWorkspace, testBase, testHead}
	output, err := execute(t, operations, append([]string{affectedCommandName}, args...)...)
	if err != nil || output != testFalseOutput ||
		!reflect.DeepEqual(operations.affectedArgs, args) {
		t.Fatalf(
			"affected called with %v, output %q, error %v",
			operations.affectedArgs,
			output,
			err,
		)
	}
}

func TestQueueRejectsInvalidArgumentsBeforeCallingOperations(t *testing.T) {
	for _, scenario := range []struct {
		name string
		args []string
		want error
	}{
		{name: "missing checkout", args: []string{matrixDeployMode}},
		{name: "missing matrix mode", args: []string{matrixCommandName, testCheckout}},
		{
			name: "invalid matrix mode",
			args: []string{matrixCommandName, testCheckout, testInvalidMode},
			want: errMatrixMode,
		},
		{
			name: "missing head",
			args: []string{affectedCommandName, testCheckout, testWorkspace, testBase},
		},
	} {
		t.Run(
			scenario.name,
			func(t *testing.T) {
				operations := &recordingOperations{}

				output, err := execute(t, operations, scenario.args...)
				if err == nil || (scenario.want != nil && !errors.Is(err, scenario.want)) {
					t.Fatalf("invalid arguments returned error %v", err)
				}

				if output != "" || !reflect.DeepEqual(*operations, recordingOperations{}) {
					t.Fatalf(
						"invalid arguments called operations %v or wrote %q",
						operations,
						output,
					)
				}
			},
		)
	}
}

func TestQueueHelpFlagShowsUsage(t *testing.T) {
	operations := &recordingOperations{}

	output, err := execute(t, operations, testHelpFlag)
	if err != nil || !strings.Contains(output, rootUsage) || operations.deployArgs != nil {
		t.Fatalf(
			"help called deployment with %v, output %q, error %v",
			operations.deployArgs,
			output,
			err,
		)
	}
}
