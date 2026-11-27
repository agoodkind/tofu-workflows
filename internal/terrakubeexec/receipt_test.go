package terrakubeexec

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestReceiptUsesItsOwnLineAfterFragmentedSubprocessOutput(t *testing.T) {
	var log bytes.Buffer
	output := &subprocessOutput{destination: &log}

	for _, fragment := range []string{
		"first\r",
		"CONFIGS_TERRAKUBE_",
		"DEPLOYMENT_RESULT {\"outcome\":\"plan_failed\"}\n",
		"TERRAKUBE_DEPLOYMENT_RESULT incomplete",
	} {
		if _, err := fmt.Fprint(output, fragment); err != nil {
			t.Fatal(err)
		}
	}

	if err := output.finish(); err != nil {
		t.Fatal(err)
	}

	receipt := deploymentReceipt{
		Version:     1,
		JobID:       "123",
		WorkspaceID: "00000000-0000-4000-8000-000000000001",
		CommitID:    "1111111111111111111111111111111111111111",
		Outcome:     "plan_failed",
	}
	if err := receipt.write(&log); err != nil {
		t.Fatal(err)
	}

	var receipts int

	for _, line := range strings.FieldsFunc(
		log.String(),
		func(char rune) bool { return char == '\n' || char == '\r' },
	) {
		if strings.HasPrefix(line, receiptPrefix) {
			receipts++
		}
	}

	if receipts != 1 || !strings.Contains(log.String(), "\"job_id\":\"123\"") {
		t.Fatalf("expected one trusted, job-bound receipt: %q", log.String())
	}
}
