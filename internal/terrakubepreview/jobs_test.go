package terrakubepreview

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type exchange func(*http.Request) (*http.Response, error)

func (f exchange) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPreviewCommit(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name, status, actual   string
		exists, stale, success bool
	}{
		{"submit", "completed", head, false, false, true},
		{"reuse", "completed", head, true, false, true},
		{"other commit", "completed", "other", true, false, false},
		{"stale request", "completed", head, false, true, false},
		{"failed", "failed", head, true, false, false},
		{"unknown", "unknown", head, true, false, false},
		{"skipped plan", "completed", head, true, false, false},
		{"missing artifact", "completed", head, true, false, false},
		{"duplicate after submission", "completed", head, false, false, false},
		{"ambiguous response", "completed", head, false, false, false},
		{"changed step job", "completed", head, true, false, false},
		{"unexpected plan step", "completed", head, true, false, false},
		{"missing step", "completed", head, true, false, false},
	} {
		t.Run(
			tc.name,
			func(t *testing.T) {
				var item job
				item.Type = "job"
				item.ID = "1"
				item.Attributes.SHA = tc.actual
				item.Attributes.Status = jobState(tc.status)
				item.Attributes.Marker = "marker"
				item.Attributes.Template = "template"
				item.Relationships.Workspace.Data = identity{Type: "workspace", ID: "workspace"}
				item.Attributes.Plan = "private/plan.bin"
				if tc.name == "missing artifact" {
					item.Attributes.Plan = ""
				}

				var evidence step
				evidence.Type = "step"
				evidence.ID = "step"
				evidence.Attributes.Number = planStepNumber
				evidence.Attributes.Name = planStepName
				evidence.Attributes.Status = completed
				evidence.Relationships.Job.Data = identity{Type: "job", ID: item.ID}
				if tc.name == "skipped plan" {
					evidence.Attributes.Status = "notExecuted"
				}

				if tc.name == "changed step job" {
					evidence.Relationships.Job.Data.ID = "another"
				}

				if tc.name == "unexpected plan step" {
					evidence.Attributes.Name = "other"
				}

				posted := false
				client := &http.Client{
					Transport: exchange(func(r *http.Request) (*http.Response, error) {
						var data any
						switch r.Method {
						case "GET":
							data = []job{}
							if tc.exists || posted {
								data = []job{item}
							}

							if tc.name == "duplicate after submission" && posted {
								data = []job{item, item}
							}

							if r.URL.Path == "/organization//job/1/step" {
								data = []step{evidence}
								if tc.name == "missing step" {
									data = []step{}
								}
							}
						case "POST":
							if posted {
								t.Fatal("retried a job submission")
							}

							if tc.name == "ambiguous response" {
								posted = true

								return nil, io.ErrUnexpectedEOF
							}

							posted = true
							var request struct{ Data job }
							if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
								t.Fatal(err)
							}

							if request.Data.Attributes.SHA != head {
								t.Fatal("submission used a different commit")
							}

							data = item
						default:
							t.Fatal(r.Method)
						}

						b, err := json.Marshal(map[string]any{"data": data})
						if err != nil {
							t.Fatal(err)
						}

						return &http.Response{
							StatusCode: 200,
							Body:       io.NopCloser(strings.NewReader(string(b))),
						}, nil
					}),
				}
				api := api{base: "https://preview.invalid", template: "template", client: client}
				err := api.plan(
					t.Context(),
					"workspace",
					head,
					"marker",
					func() error {
						if tc.stale {
							return errors.New("head changed")
						}

						return nil
					},
					func(context.Context) error {
						t.Fatal("unexpected poll")

						return nil
					},
				)
				if (err == nil) != tc.success {
					t.Fatalf("unexpected outcome: %v", err)
				}

				if posted != (!tc.exists && !tc.stale) {
					t.Fatalf("unexpected submission: %t", posted)
				}
			},
		)
	}
}
