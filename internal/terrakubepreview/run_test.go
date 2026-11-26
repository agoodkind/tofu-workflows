package terrakubepreview

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-github/v93/github"
)

func TestReportBindsCheckToCurrentHeadAndBase(t *testing.T) {
	for _, scenario := range []string{
		"current",
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
