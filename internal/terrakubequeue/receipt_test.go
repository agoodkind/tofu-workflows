package terrakubequeue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/agoodkind/tofu-workflows/internal/terrakubequeue/reconcile"
)

const (
	receiptInitialCommit      = "initial"
	receiptNextCommit         = "next"
	receiptTestAPIURL         = "https://example.invalid/api/v1"
	receiptTestJobsPath       = "/api/v1/organization/" + receiptTestOrganizationID + "/job"
	receiptTestStepsPath      = receiptTestJobsPath + "/19/step"
	receiptTestOutputPath     = "/tfoutput/v1/organization/" + receiptTestOrganizationID + "/job/19/step/" + receiptTestStepID
	receiptTestStepID         = "00000000-0000-4000-8000-000000000004"
	receiptTestWorkspaceID    = "00000000-0000-4000-8000-000000000002"
	receiptTestOrganizationID = "00000000-0000-4000-8000-000000000001"
	receiptTestCommit         = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	receiptTestMarker         = receiptPrefix + `{"version":1,"job_id":"19","workspace_id":"` + receiptTestWorkspaceID + `","commit_id":"` + receiptTestCommit + `","outcome":"plan_failed"}`
)

type receiptSource struct {
	current string
}

func (source *receiptSource) Promotion(context.Context) (string, error) {
	return source.current, nil
}

func (source *receiptSource) NextCommit(_ context.Context, current string) (string, error) {
	if current == receiptNextCommit {
		return "", nil
	}

	return receiptNextCommit, nil
}

func (source *receiptSource) Promote(_ context.Context, current, next string) error {
	if source.current != current {
		return errors.New("ref changed")
	}

	source.current = next

	return nil
}

type receiptTransport func(*http.Request) (*http.Response, error)

func (transport receiptTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return transport(req)
}

func TestFailedJobNeedsOneMatchingReceiptFromItsCompleteStepLog(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		incomplete   bool
		advance      bool
	}{
		{"plan failure", receiptTestMarker, false, true},
		{"provider output", "| " + receiptTestMarker, false, false},
		{
			"wrong commit",
			strings.Replace(receiptTestMarker, receiptTestCommit, strings.Repeat("b", 40), 1),
			false,
			false,
		},
		{"wrong job", strings.Replace(receiptTestMarker, `"19"`, `"20"`, 1), false, false},
		{
			"wrong workspace",
			strings.Replace(
				receiptTestMarker,
				receiptTestWorkspaceID,
				receiptTestOrganizationID,
				1,
			),
			false,
			false,
		},
		{"apply failed", "apply returned exit 1", false, false},
		{"duplicate", receiptTestMarker + "\n" + receiptTestMarker, false, false},
		{"invalid", receiptPrefix + "{", false, false},
		{"partial", receiptTestMarker, true, false},
	} {
		t.Run(
			tc.name,
			func(t *testing.T) {
				source := &receiptSource{current: receiptTestCommit}
				issuedTokens := 0
				requests := 0
				client := apiClient{
					BaseURL:      receiptTestAPIURL,
					Organization: receiptTestOrganizationID,
					Workspace:    receiptTestWorkspaceID,
					Template:     receiptTestStepID,

					Token: func(_ context.Context, audience string) (string, error) {
						if audience != deploymentAudience {
							t.Fatal(audience)
						}

						issuedTokens++

						return fmt.Sprintf("%s-%d", t.Name(), issuedTokens), nil
					},
				}
				jobs := 0
				client.Client = &http.Client{
					Transport: receiptTransport(func(req *http.Request) (*http.Response, error) {
						requests++
						if req.Header.Get("Authorization") != fmt.Sprintf(
							"Bearer %s-%d",
							t.Name(),
							requests,
						) {
							t.Fatal("missing scoped authorization")
						}

						var body string
						headers := http.Header{}
						switch req.URL.Path {
						case receiptTestJobsPath:
							if req.Method == http.MethodPost {
								jobs++
								body = `{"data":{"id":"20","type":"job"}}`

								break
							}

							body = `{"data":[{"type":"job","id":"19","attributes":{"status":"failed","comments":"deployment-main-` + receiptTestCommit + `","commitId":"` + receiptTestCommit + `","templateReference":"` + receiptTestStepID + `"},"relationships":{"workspace":{"data":{"id":"` + receiptTestWorkspaceID + `","type":"workspace"}}}}]}`
						case receiptTestStepsPath:
							body = `{"data":[{"id":"` + receiptTestStepID + `","type":"step","attributes":{"status":"failed"}}]}`
						case receiptTestOutputPath:
							body = tc.output
							if tc.incomplete {
								headers.Set("X-Terrakube-Output-Incomplete", "true")
							}
						default:
							t.Fatalf("unexpected %s", req.URL.Path)
						}

						return &http.Response{
							StatusCode: http.StatusOK,
							Body:       io.NopCloser(strings.NewReader(body)),
							Header:     headers,
						}, nil
					}),
				}
				_, err := reconcile.Step(t.Context(), receiptInitialCommit, source, client)
				if tc.advance {
					if err != nil || jobs != 1 || source.current != receiptNextCommit {
						t.Fatalf("expected advancement: %v, %d submissions", err, jobs)
					}
				} else if !errors.Is(
					err,
					reconcile.ErrReconcileApply,
				) || jobs != 0 || source.current != receiptTestCommit {
					t.Fatalf("expected reconciliation: %v, %d submissions", err, jobs)
				}
			},
		)
	}
}
