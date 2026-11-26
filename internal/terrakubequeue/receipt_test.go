package terrakubequeue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type receiptTransport func(*http.Request) (*http.Response, error)

func (transport receiptTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return transport(req)
}

func TestFailedJobNeedsOneMatchingReceiptFromItsCompleteStepLog(t *testing.T) {
	const step = "00000000-0000-4000-8000-000000000004"
	const workspace = "00000000-0000-4000-8000-000000000002"
	const org = "00000000-0000-4000-8000-000000000001"
	const commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	const marker = receiptPrefix + `{"version":1,"job_id":"19","workspace_id":"` + workspace + `","commit_id":"` + commit + `","outcome":"plan_failed"}`
	for _, tc := range []struct {
		name, output string
		incomplete   bool
		advance      bool
	}{
		{"plan failure", marker, false, true},
		{"provider output", "| " + marker, false, false},
		{"wrong commit", strings.Replace(marker, commit, strings.Repeat("b", 40), 1), false, false},
		{"wrong job", strings.Replace(marker, `"19"`, `"20"`, 1), false, false},
		{"wrong workspace", strings.Replace(marker, workspace, org, 1), false, false},
		{"apply failed", "apply returned exit 1", false, false},
		{"duplicate", marker + "\n" + marker, false, false},
		{"invalid", receiptPrefix + "{", false, false},
		{"partial", marker, true, false},
	} {
		t.Run(
			tc.name,
			func(t *testing.T) {
				source := &state{current: commit, next: "next"}
				issuedTokens := 0
				requests := 0
				client := apiClient{
					BaseURL:      "https://example.invalid/api/v1",
					Organization: org,
					Workspace:    workspace,
					Template:     step,

					Token: func(_ context.Context, audience string) (string, error) {
						if audience != "terrakube-configs-deploy" {
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
						case "/api/v1/organization/" + org + "/job":
							if req.Method == "POST" {
								jobs++
								body = `{"data":{"id":"20","type":"job"}}`

								break
							}

							body = `{"data":[{"type":"job","id":"19","attributes":{"status":"failed","comments":"configs-main-` + commit + `","commitId":"` + commit + `","templateReference":"` + step + `"},"relationships":{"workspace":{"data":{"id":"` + workspace + `","type":"workspace"}}}}]}`
						case "/api/v1/organization/" + org + "/job/19/step":
							body = `{"data":[{"id":"` + step + `","type":"step","attributes":{"status":"failed"}}]}`
						case "/tfoutput/v1/organization/" + org + "/job/19/step/" + step:
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
				_, err := wake(context.Background(), "initial", source, client)
				if tc.advance {
					if err != nil || jobs != 1 || source.current != "next" {
						t.Fatalf("expected advancement: %v, %d submissions", err, jobs)
					}
				} else if !errors.Is(
					err,
					errReconcileApply,
				) || jobs != 0 || source.current != commit {
					t.Fatalf("expected reconciliation: %v, %d submissions", err, jobs)
				}
			},
		)
	}
}
