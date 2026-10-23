package planjson_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v93/github"
	tfjson "github.com/hashicorp/terraform-json"
)

func tofu(t *testing.T, directory string, environment []string, args ...string) []byte {
	t.Helper()
	command := exec.CommandContext(t.Context(), "tofu", args...)
	command.Dir = directory
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GITHUB_TOKEN=") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, environment...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("OpenTofu %s failed: %v; captured output is private", args[0], err)
	}
	return stdout.Bytes()
}

func plan(t *testing.T, directory string, environment []string, name string) *tfjson.Plan {
	t.Helper()
	tofu(t, directory, environment, "plan", "-input=false", "-no-color", "-out="+name)
	raw := tofu(t, directory, environment, "show", "-json", name)
	result := new(tfjson.Plan)
	result.UseJSONNumber(true)
	if err := json.Unmarshal(raw, result); err != nil {
		t.Fatalf("The library rejected real OpenTofu plan JSON: %v", err)
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("The library rejected the plan format: %v", err)
	}
	if result.TerraformVersion != "1.13.2" {
		t.Fatalf("Unexpected OpenTofu version: %s", result.TerraformVersion)
	}
	var original map[string]any
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatal("The source JSON could not be checked")
	}
	for _, entry := range original["resource_changes"].([]any) {
		resource := entry.(map[string]any)
		decoded := change(t, result, resource["address"].(string))
		originalChange := resource["change"].(map[string]any)
		for field, actual := range map[string]any{"before_sensitive": decoded.BeforeSensitive, "after_sensitive": decoded.AfterSensitive, "after_unknown": decoded.AfterUnknown} {
			if !reflect.DeepEqual(originalChange[field], actual) {
				t.Fatalf("The library changed the original %s mask", field)
			}
		}
	}
	t.Logf("terraform-json accepted OpenTofu %s plan format %s", result.TerraformVersion, result.FormatVersion)
	return result
}

func change(t *testing.T, plan *tfjson.Plan, address string) *tfjson.Change {
	t.Helper()
	for _, resource := range plan.ResourceChanges {
		if resource.Address == address {
			return resource.Change
		}
	}
	t.Fatalf("The real plan omitted resource %s", address)
	return nil
}

func pathMarked(mask any, path ...any) bool {
	for _, part := range path {
		if marked, ok := mask.(bool); ok && marked {
			return true
		}
		switch index := part.(type) {
		case string:
			object, ok := mask.(map[string]any)
			if !ok {
				return false
			}
			mask = object[index]
		case int:
			array, ok := mask.([]any)
			if !ok || index >= len(array) {
				return false
			}
			mask = array[index]
		}
	}
	marked, ok := mask.(bool)
	return ok && marked
}

func child(value any, key string) any {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return object[key]
}
func element(value any, index int) any {
	array, ok := value.([]any)
	if !ok || index >= len(array) {
		return nil
	}
	return array[index]
}
func redact(value, sensitive, unknown any) any {
	if marked, ok := sensitive.(bool); ok && marked {
		return "<sensitive>"
	}
	if marked, ok := unknown.(bool); ok && marked {
		return "<unknown>"
	}
	switch current := value.(type) {
	case map[string]any:
		keys := map[string]struct{}{}
		for _, source := range []any{current, sensitive, unknown} {
			if object, ok := source.(map[string]any); ok {
				for key := range object {
					keys[key] = struct{}{}
				}
			}
		}
		result := map[string]any{}
		for key := range keys {
			result[key] = redact(current[key], child(sensitive, key), child(unknown, key))
		}
		return result
	case []any:
		result := make([]any, len(current))
		for index, item := range current {
			result[index] = redact(item, element(sensitive, index), element(unknown, index))
		}
		return result
	default:
		return value
	}
}

func assertRedaction(t *testing.T, value *tfjson.Plan, confidential string) {
	t.Helper()
	visible := make(map[string]any)
	markedValues := map[string]struct{}{}
	for _, resource := range value.ResourceChanges {
		collectMarkedStrings(resource.Change.Before, resource.Change.BeforeSensitive, markedValues)
		collectMarkedStrings(resource.Change.After, resource.Change.AfterSensitive, markedValues)
		visible[resource.Address] = map[string]any{"before": redact(resource.Change.Before, resource.Change.BeforeSensitive, nil), "after": redact(resource.Change.After, resource.Change.AfterSensitive, resource.Change.AfterUnknown)}
	}
	for name, output := range value.OutputChanges {
		collectMarkedStrings(output.Before, output.BeforeSensitive, markedValues)
		collectMarkedStrings(output.After, output.AfterSensitive, markedValues)
		visible["output."+name] = redact(output.After, output.AfterSensitive, output.AfterUnknown)
	}
	if paths := matchingPaths(visible, confidential, ""); len(paths) > 0 {
		t.Logf("Original masks leave copied values at %v; apply known-value redaction before formatting", paths)
	}
	values := make([]string, 0, len(markedValues))
	for value := range markedValues {
		values = append(values, value)
	}
	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
	for key, item := range visible {
		visible[key] = redactKnownStrings(item, values)
	}
	var rendered bytes.Buffer
	encoder := json.NewEncoder(&rendered)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(visible); err != nil {
		t.Fatal("The redacted projection could not be serialized")
	}
	display := rendered.Bytes()
	if bytes.Contains(display, []byte(confidential)) {
		t.Fatalf("The redacted projection retains the synthetic value at field paths %v", matchingPaths(visible, confidential, ""))
	}
	if !bytes.Contains(display, []byte("visible-item")) || !bytes.Contains(display, []byte("<sensitive>")) || !bytes.Contains(display, []byte("<unknown>")) {
		t.Fatal("The projection did not preserve public values, sensitivity, and unknown markers")
	}
}

func collectMarkedStrings(value, mask any, values map[string]struct{}) {
	marked, _ := mask.(bool)
	switch current := value.(type) {
	case string:
		if marked && current != "" {
			values[current] = struct{}{}
		}
	case map[string]any:
		for key, item := range current {
			next := child(mask, key)
			if marked {
				next = true
			}
			collectMarkedStrings(item, next, values)
		}
	case []any:
		for index, item := range current {
			next := element(mask, index)
			if marked {
				next = true
			}
			collectMarkedStrings(item, next, values)
		}
	}
}

func redactKnownStrings(value any, values []string) any {
	switch current := value.(type) {
	case string:
		for _, sensitive := range values {
			current = strings.ReplaceAll(current, sensitive, "<sensitive>")
		}
		return current
	case map[string]any:
		result := map[string]any{}
		for key, item := range current {
			result[key] = redactKnownStrings(item, values)
		}
		return result
	case []any:
		result := make([]any, len(current))
		for index, item := range current {
			result[index] = redactKnownStrings(item, values)
		}
		return result
	default:
		return value
	}
}

func matchingPaths(value any, confidential, path string) []string {
	var found []string
	switch current := value.(type) {
	case string:
		if strings.Contains(current, confidential) {
			found = append(found, path)
		}
	case map[string]any:
		for key, item := range current {
			found = append(found, matchingPaths(item, confidential, path+"."+key)...)
		}
	case []any:
		for index, item := range current {
			found = append(found, matchingPaths(item, confidential, path+"["+strconv.Itoa(index)+"]")...)
		}
	}
	return found
}

func TestRealOpenTofuPlansPreserveActionsMasksAndUnknowns(t *testing.T) {
	directory := t.TempDir()
	source, err := os.ReadFile("fixture/main.tf")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "main.tf"), source, 0600); err != nil {
		t.Fatal(err)
	}
	random := make([]byte, 24)
	if _, err = rand.Read(random); err != nil {
		t.Fatal(err)
	}
	confidential := hex.EncodeToString(random)
	firstEnvironment := []string{"TF_IN_AUTOMATION=1", "TF_VAR_stage=1", "TF_VAR_confidential=" + confidential}
	tofu(t, directory, firstEnvironment, "init", "-input=false", "-no-color")
	first := plan(t, directory, firstEnvironment, "first.tfplan")
	if len(first.ResourceChanges) != 6 {
		t.Fatalf("Initial plan has %d resource changes; expected six", len(first.ResourceChanges))
	}
	for _, resource := range first.ResourceChanges {
		if !resource.Change.Actions.Create() {
			t.Fatalf("Initial resource %s does not have create action", resource.Address)
		}
	}
	firstApp := change(t, first, "terraform_data.app")
	if original, ok := child(child(child(firstApp.After, "input"), "nested"), "private").(string); !ok || original != confidential {
		t.Fatal("The library did not preserve the synthetic value before redaction")
	}
	if !pathMarked(firstApp.AfterUnknown, "id") || !pathMarked(firstApp.AfterUnknown, "output") {
		t.Fatal("The library omitted initial computed unknown markers")
	}
	if !pathMarked(first.OutputChanges["computed_id"].AfterUnknown) {
		t.Fatal("The computed output lost its unknown marker")
	}
	if !pathMarked(firstApp.AfterSensitive, "input", "nested", "private") || !pathMarked(firstApp.AfterSensitive, "input", "entries", 0, "private") {
		t.Fatal("Nested object or list sensitivity masks are missing")
	}
	if !pathMarked(first.OutputChanges["sensitive_result"].AfterSensitive) {
		t.Fatal("The sensitive output marker is missing")
	}
	assertRedaction(t, first, confidential)
	tofu(t, directory, firstEnvironment, "apply", "-input=false", "-no-color", "first.tfplan")
	secondEnvironment := []string{"TF_IN_AUTOMATION=1", "TF_VAR_stage=2", "TF_VAR_confidential=" + confidential}
	second := plan(t, directory, secondEnvironment, "second.tfplan")
	if !change(t, second, "terraform_data.app").Actions.Update() {
		t.Fatal("The library did not preserve the real update action")
	}
	if !change(t, second, "terraform_data.delete_me[0]").Actions.Delete() {
		t.Fatal("The library did not preserve the real delete action")
	}
	if !change(t, second, "terraform_data.create_me[0]").Actions.Create() {
		t.Fatal("The library did not preserve the real create action")
	}
	for address, createFirst := range map[string]bool{"terraform_data.replace_destroy_first": false, "terraform_data.replace_create_first": true} {
		item := change(t, second, address)
		if !item.Actions.Replace() || item.Actions.CreateBeforeDestroy() != createFirst || item.Actions.DestroyBeforeCreate() == createFirst || len(item.ReplacePaths) == 0 {
			t.Fatalf("Replacement ordering or paths are missing for %s", address)
		}
	}
	secondApp := change(t, second, "terraform_data.app")
	if !pathMarked(secondApp.BeforeSensitive, "input", "nested", "private") || !pathMarked(secondApp.AfterSensitive, "input", "entries", 0, "private") {
		t.Fatal("The update did not preserve before and after sensitivity masks")
	}
	if !pathMarked(secondApp.AfterUnknown, "output") {
		t.Fatal("The update lost its computed output marker")
	}
	protected := map[string]struct{}{"terraform_data.protected": {}}
	blocked := []string{}
	for _, resource := range second.ResourceChanges {
		if _, found := protected[resource.Address]; found && !resource.Change.Actions.NoOp() {
			blocked = append(blocked, resource.Address)
		}
	}
	if !slices.Equal(blocked, []string{"terraform_data.protected"}) || !change(t, second, "terraform_data.protected_neighbor").Actions.NoOp() {
		t.Fatal("Exact protected-resource detection did not distinguish the neighboring resource")
	}
	protectedAfter := change(t, second, "terraform_data.protected").After.(map[string]any)
	if number, ok := protectedAfter["input"].(json.Number); !ok || number.String() != "2" {
		t.Fatal("UseJSONNumber did not preserve the numeric value")
	}
	assertRedaction(t, second, confidential)
	tofu(t, directory, secondEnvironment, "apply", "-input=false", "-no-color", "second.tfplan")
	final := plan(t, directory, secondEnvironment, "final.tfplan")
	for _, resource := range final.ResourceChanges {
		if !resource.Change.Actions.NoOp() {
			t.Fatalf("Applied resource %s still changes", resource.Address)
		}
	}
	t.Log("Real create, update, delete, both replacement orders, sensitive object and list values, sensitive output, unknown values, numeric precision, exact protected addresses, and final no-op checks passed.")
}

func TestGitHubSDKReadsCurrentRunAndRepository(t *testing.T) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		t.Fatal("GITHUB_TOKEN is required for the read-only SDK proof")
	}
	repository := strings.Split(os.Getenv("GITHUB_REPOSITORY"), "/")
	if len(repository) != 2 {
		t.Fatal("GITHUB_REPOSITORY must identify the current repository")
	}
	runID, err := strconv.ParseInt(os.Getenv("GITHUB_RUN_ID"), 10, 64)
	if err != nil {
		t.Fatal("GITHUB_RUN_ID is invalid")
	}
	client, err := github.NewClient(github.WithAuthToken(token), github.WithHTTPClient(&http.Client{Timeout: 20 * time.Second}))
	if err != nil {
		t.Fatal("The pinned SDK client could not initialize")
	}
	run, _, err := client.Actions.GetWorkflowRunByID(t.Context(), repository[0], repository[1], runID)
	if err != nil {
		t.Fatal("The SDK could not read the current workflow run")
	}
	if run.GetID() != runID || run.GetHeadSHA() != os.Getenv("GITHUB_SHA") {
		t.Fatal("The SDK returned a different workflow identity")
	}
	repo, _, err := client.Repositories.Get(t.Context(), repository[0], repository[1])
	if err != nil {
		t.Fatal("The SDK could not read current repository metadata")
	}
	if repo.GetFullName() != os.Getenv("GITHUB_REPOSITORY") {
		t.Fatal("The SDK returned a different repository")
	}
	t.Log("The pinned SDK authenticated read-only requests for the exact current run, commit, and repository.")
}
