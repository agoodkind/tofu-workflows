package proof

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func readOutput(t *testing.T, variable string) []byte {
	t.Helper()
	path := os.Getenv(variable)
	if path == "" {
		t.Fatalf("required output path %s is missing", variable)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", variable, err)
	}
	if len(data) == 0 {
		t.Fatalf("output %s is empty", variable)
	}
	return data
}

func TestPlan(t *testing.T) {
	if os.Getenv("PLAN_CHANGED") != "true" {
		t.Fatal("the plan did not report the expected resource changes")
	}
	readOutput(t, "PLAN_BINARY")
	var plan struct {
		Changes []json.RawMessage `json:"resource_changes"`
	}
	if err := json.Unmarshal(readOutput(t, "PLAN_JSON"), &plan); err != nil {
		t.Fatalf("parse plan JSON: %v", err)
	}
	if len(plan.Changes) != 2 {
		t.Fatalf("resource changes = %d, expected 2", len(plan.Changes))
	}
	if bytes.Contains(readOutput(t, "PLAN_TEXT"), []byte("synthetic-value-for-redaction-check")) {
		t.Fatal("the readable plan disclosed the sensitive fixture value")
	}
}

func TestApply(t *testing.T) {
	var outputs map[string]string
	if err := json.Unmarshal(readOutput(t, "APPLY_OUTPUT"), &outputs); err != nil {
		t.Fatalf("parse apply output: %v", err)
	}
	if outputs["observed_release"] != "release-one" {
		t.Fatal("the apply did not record the expected release")
	}
}

func TestStalePlan(t *testing.T) {
	if os.Getenv("STALE_OUTCOME") != "failure" {
		t.Fatal("the stale saved plan did not fail")
	}
	if os.Getenv("STALE_REASON") != "apply-failed" {
		t.Fatal("the action did not classify the stale-plan rejection as an apply failure")
	}
}
