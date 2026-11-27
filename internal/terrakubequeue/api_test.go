package terrakubequeue

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

const (
	canonicalWorkspaceUUID = "abcdef01-2345-4000-8000-abcdef012345"
	expectedTestOrigin     = "https://github.com/example/configuration"
	differentTestOrigin    = "https://github.com/example/other"
	testPromotionBranch    = "terrakube/example"
	unusedCheckout         = "unused-checkout"
	deploymentName         = "example"
	invalidURL             = "https://github.com/example/configuration"
	invalidPath            = "example/.."
	invalidOwnerAndRepo    = "example/a/b"
)

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

func TestWorkspaceUsesExpectedRepository(t *testing.T) {
	for _, test := range []struct {
		source string
		want   error
	}{
		{source: expectedTestOrigin},
		{source: expectedTestOrigin + ".git"},
		{source: differentTestOrigin, want: errWorkspaceSource},
	} {
		client := apiClient{
			BaseURL:      receiptTestAPIURL,
			Organization: canonicalWorkspaceUUID,
			Workspace:    canonicalWorkspaceUUID,
			Template:     canonicalWorkspaceUUID,
			Token:        func(context.Context, string) (string, error) { return "token", nil },
			Client: &http.Client{
				Transport: receiptTransport(func(_ *http.Request) (*http.Response, error) {
					body := `{"data":{"type":"workspace","id":"` + canonicalWorkspaceUUID +
						`","attributes":{"branch":"` + testPromotionBranch +
						`","source":"` + test.source + `","defaultTemplate":"` + canonicalWorkspaceUUID +
						`","restrictApplyToConfiguredBranch":true}}}`

					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader(body)),
					}, nil
				}),
			},
		}
		if err := client.verifyWorkspace(
			t.Context(),
			testPromotionBranch,
			expectedTestOrigin,
		); !errors.Is(err, test.want) {
			t.Errorf("verify workspace source %s: got %v, want %v", test.source, err, test.want)
		}
	}
}

func TestDeploymentRejectsInvalidRepositoryBeforeOpeningCheckout(t *testing.T) {
	t.Setenv(promotionEnvironmentEnv, deploymentName)
	enabled := true
	entries := []target{{Name: deploymentName, Environment: deploymentName, Enabled: &enabled}}

	for _, repository := range []string{"", invalidURL, invalidPath, invalidOwnerAndRepo} {
		t.Setenv(promotionRepositoryEnv, repository)

		err := runDeployment(
			t.Context(),
			[]string{"queue", deploymentName, unusedCheckout},
			entries,
		)
		if !errors.Is(err, errPromotionRepository) {
			t.Errorf(
				"run deployment with repository %q: got %v, want invalid repository error",
				repository,
				err,
			)
		}
	}
}
