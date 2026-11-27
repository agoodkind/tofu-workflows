package terrakubequeue

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const canonicalWorkspaceUUID = "abcdef01-2345-4000-8000-abcdef012345"

func TestAPIRejectsNoncanonicalUUIDs(t *testing.T) {
	client := apiClient{
		BaseURL:      receiptTestAPIURL,
		Organization: canonicalWorkspaceUUID,
		Workspace:    canonicalWorkspaceUUID,
		Template:     canonicalWorkspaceUUID,
		Token:        func(context.Context, string) (string, error) { return "token", nil },
	}
	if err := client.validate(); err != nil {
		t.Fatal(err)
	}

	for _, value := range []string{
		strings.ToUpper(canonicalWorkspaceUUID),
		strings.ReplaceAll(canonicalWorkspaceUUID, "-", ""),
		"urn:uuid:" + canonicalWorkspaceUUID,
		"{" + canonicalWorkspaceUUID + "}",
		"00000000-0000-0000-0000-00000000000z",
	} {
		client.Workspace = value
		if err := client.validate(); !errors.Is(err, errMissingAPISettings) {
			t.Errorf("validate workspace %q: got %v, want an invalid UUID error", value, err)
		}
	}
}
