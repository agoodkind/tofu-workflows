package terrakubepreview

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-github/v93/github"
)

func TestReportBindsCheckToCurrentHeadAndBase(t *testing.T) {
	for _, scenario := range []string{
		"current",
		"unaffected",
		"head behind main",
		"main advanced during plan",
		"plan timeout",
	} {
		t.Run(
			scenario,
			func(t *testing.T) {
				t.Setenv("TERRAKUBE_PREVIEW_API_URL", "https://api.invalid")
				t.Setenv("TERRAKUBE_PREVIEW_UI_URL", "https://ui.invalid")
				const base = "base"
				const head = "head"
				ran := false
				conclusion := ""
				client, err := github.NewClient(
					github.WithHTTPClient(&http.Client{
						Transport: exchange(func(req *http.Request) (*http.Response, error) {
							result := "{}"
							switch {
							case strings.HasSuffix(req.URL.Path, "/pulls/7"):
								currentBase := base
								if ran && scenario == "main advanced during plan" {
									currentBase = "new"
								}

								result = `{"state":"open","base":{"ref":"main","repo":{"full_name":"agoodkind/configs"},"sha":"` + currentBase + `"},"head":{"repo":{"full_name":"agoodkind/configs"},"sha":"head"}}`
							case strings.HasSuffix(req.URL.Path, "/compare/base...head"):
								result = `{"merge_base_commit":{"sha":"base"}}`
								if scenario == "head behind main" {
									result = `{"merge_base_commit":{"sha":"ancestor"}}`
								}
							case req.Method == "POST":
								var check github.CreateCheckRunOptions
								if err := json.NewDecoder(req.Body).Decode(&check); err != nil {
									t.Fatal(err)
								}

								if check.HeadSHA != head || check.Name != checkName {
									t.Fatalf("incorrect check binding: %#v", check)
								}

								result = `{"id":10}`
							case req.Method == "PATCH":
								var check github.UpdateCheckRunOptions
								if err := json.NewDecoder(req.Body).Decode(&check); err != nil {
									t.Fatal(err)
								}

								conclusion = *check.Conclusion
							default:
								t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
							}

							return &http.Response{
								StatusCode: 200,
								Header:     http.Header{"Content-Type": {"application/json"}},
								Body:       io.NopCloser(strings.NewReader(result)),
							}, nil
						}),
					}),
				)
				if err != nil {
					t.Fatal(err)
				}

				err = report(
					t.Context(),
					client,
					client,
					7,
					base,
					head,
					func(
						ctx context.Context,
						_ string,
						_ int,
						gotBase string,
						gotHead string,
						_ func() error,
					) error {
						ran = true
						if _, ok := ctx.Deadline(); !ok || gotBase != base || gotHead != head {
							t.Fatal("plan deadline or revision binding missing")
						}

						if scenario == "unaffected" {
							return errUnaffected
						}

						if scenario == "plan timeout" {
							return context.DeadlineExceeded
						}

						return nil
					},
				)
				switch scenario {
				case "current":
					if err != nil || conclusion != "success" {
						t.Fatalf("current revision: %v, %s", err, conclusion)
					}
				case "unaffected":
					if err != nil || conclusion != "neutral" {
						t.Fatalf("unaffected: %v %s", err, conclusion)
					}
				case "head behind main":
					if err == nil || ran || conclusion != "" {
						t.Fatal("allowed stale base")
					}
				default:
					if err == nil || conclusion != "failure" {
						t.Fatalf("did not report terminal failure: %v %s", err, conclusion)
					}
				}
			},
		)
	}
}

func TestSelectAffectedWorkspaceRevisions(t *testing.T) {
	const qa = "00000000-0000-0000-0000-000000000001"
	const prod = "00000000-0000-0000-0000-000000000002"
	t.Setenv("TERRAKUBE_PREVIEW_QA_ID", qa)
	t.Setenv("TERRAKUBE_PREVIEW_PROD_ID", prod)

	for _, tc := range []struct {
		name             string
		qa, prod, failed bool
		want             []string
	}{
		{"QA only", true, false, false, []string{qa}},
		{"production only", false, true, false, []string{prod}},
		{"shared", true, true, false, []string{qa, prod}},
		{"unaffected", false, false, false, nil},
		{"selector error", true, false, true, nil},
	} {
		t.Run(
			tc.name,
			func(t *testing.T) {
				roots, err := affectedWorkspaces(
					t.Context(),
					"base",
					"head",
					func(_ context.Context, root, base, head string) (bool, error) {
						if base != "base" || head != "head" {
							t.Fatal("selector received a different revision")
						}

						if root == "opentofu/suburban/tack-qa" {
							return tc.qa, nil
						}

						if root != "opentofu/vault/tack-prod" {
							t.Fatal(root)
						}

						if tc.failed {
							return false, io.ErrUnexpectedEOF
						}

						return tc.prod, nil
					},
				)
				if (err != nil) != tc.failed || !slices.Equal(roots, tc.want) {
					t.Fatalf("selected %v, error %v", roots, err)
				}
			},
		)
	}
}
