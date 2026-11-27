package terrakubequeue

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAPIRejectsNoncanonicalUUIDs(t *testing.T) {
	const id = "abcdef01-2345-4000-8000-abcdef012345"
	client := apiClient{
		BaseURL:      "https://example.invalid/api/v1",
		Organization: id,
		Workspace:    id,
		Template:     id,
		Token:        func(context.Context, string) (string, error) { return "token", nil },
	}
	if err := client.validate(); err != nil {
		t.Fatal(err)
	}

	for _, value := range []string{
		strings.ToUpper(id),
		strings.ReplaceAll(id, "-", ""),
		"urn:uuid:" + id,
		"{" + id + "}",
		"00000000-0000-0000-0000-00000000000z",
	} {
		client.Workspace = value
		if err := client.validate(); !errors.Is(err, errMissingAPISettings) {
			t.Errorf("validate workspace %q: got %v, want an invalid UUID error", value, err)
		}
	}
}
