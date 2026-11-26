package terrakubeexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestDeployUsesSavedPlanOnlyAfterPlanAndSourceChecks(t *testing.T) {
	for _, test := range []struct {
		name        string
		status      error
		verifyError error
		applyError  error
		want        []string
		fail        bool
	}{
		{
			name:   "changes",
			status: processStatusError(changesPending),
			want:   []string{"providers", "init", "plan", "verify", "apply"},
		},
		{
			name:       "apply error cannot emit a plan receipt",
			status:     processStatusError(changesPending),
			applyError: processStatusError(planFailed),
			want:       []string{"providers", "init", "plan", "verify", "apply"},
			fail:       true,
		},
		{name: "no changes", want: []string{"providers", "init", "plan"}},
		{
			name:   "plan error",
			status: processStatusError(1),
			want:   []string{"providers", "init", "plan", "receipt"},
			fail:   true,
		},
		{
			name:        "promotion changed",
			status:      processStatusError(changesPending),
			verifyError: errors.New("promotion changed"),
			want:        []string{"providers", "init", "plan", "verify"},
			fail:        true,
		},
	} {
		t.Run(
			test.name,
			func(t *testing.T) {
				var calls []string
				planPath := "/private/deployment.tfplan"
				selected := deployment{selector: "suburban/tack-qa"}

				job := operation{
					selected:     selected,
					paths:        jobPaths{root: "/private"},
					initReadOnly: true,
					output:       io.Discard,
					publishPlan:  func() error { return nil },
					failPlan: func() error {
						calls = append(calls, "receipt")

						return nil
					},
					install: func(_ context.Context, task command) error {
						calls = append(calls, task.args[0])
						if !slices.Equal(task.args, []string{"providers", "install"}) {
							t.Fatalf("unexpected provider command: %v", task.args)
						}

						return nil
					},
					verify: func(_ context.Context) error {
						calls = append(calls, "verify")

						return test.verifyError
					},
					run: func(_ context.Context, task command) error {
						subcommand := task.args[2]
						calls = append(calls, subcommand)
						switch subcommand {
						case "init":
							if !slices.Contains(task.args, "-lockfile=readonly") {
								t.Fatalf(
									"initialization did not protect the tracked lock file: %v",
									task.args,
								)
							}
						case "plan":
							if !slices.Contains(task.args, "-out="+planPath) {
								t.Fatalf("plan was not saved to %s: %v", planPath, task.args)
							}

							return test.status
						case "apply":
							if task.args[len(task.args)-1] != planPath {
								t.Fatalf("apply did not use the saved plan: %v", task.args)
							}

							return test.applyError
						}

						return nil
					},
				}

				err := job.execute(t.Context())
				if (err != nil) != test.fail || !slices.Equal(calls, test.want) {
					t.Fatalf(
						"calls = %v, error = %v; want %v, failure = %v",
						calls,
						err,
						test.want,
						test.fail,
					)
				}
			},
		)
	}
}

func TestCheckoutRejectsACommitOutsideTrustedMainFirstParent(t *testing.T) {
	const commit = "1111111111111111111111111111111111111111"
	const newer = "2222222222222222222222222222222222222222"
	const branch = "terrakube/tack-qa"

	for _, test := range []struct {
		name      string
		history   string
		tip       string
		wantError string
	}{
		{name: "first parent", history: newer + "\n" + commit, tip: commit},
		{
			name:      "only a merged side branch",
			history:   newer,
			tip:       commit,
			wantError: "first-parent history",
		},
		{
			name:      "promotion advanced",
			history:   newer + "\n" + commit,
			tip:       newer,
			wantError: "promotion branch tip",
		},
	} {
		t.Run(
			test.name,
			func(t *testing.T) {
				responses := map[string]string{
					"remote get-url origin":             configsOrigin,
					"rev-parse --verify HEAD^{commit}":  commit,
					"rev-parse --is-shallow-repository": "true",
					"fetch --no-tags --unshallow " + configsOrigin + " +refs/heads/main:" + mainRef +
						" +refs/heads/" + branch + ":refs/remotes/origin/" + branch: "",
					"rev-parse --verify refs/remotes/origin/" + branch: test.tip,
					"rev-list --first-parent " + mainRef:               test.history,
				}
				git := func(_ context.Context, task command) error {
					key := strings.Join(task.args, " ")
					response, exists := responses[key]
					if !exists {
						t.Fatalf("unexpected Git command %q", key)
					}

					_, err := fmt.Fprintln(task.output, response)

					return err
				}
				result, err := verifyCheckout(t.Context(), git, configsOrigin, branch)
				if test.wantError != "" {
					if err == nil || !strings.Contains(err.Error(), test.wantError) {
						t.Fatalf("checkout verification error = %v; want %s", err, test.wantError)
					}

					return
				}

				if err != nil || result != commit {
					t.Fatalf("checkout verification = %q, %v; want %s", result, err, commit)
				}
			},
		)
	}
}

func TestPreviewWorkspaceCannotSelectDeploymentCredentials(t *testing.T) {
	getenv := func(key string) string {
		if key != "workspaceName" {
			t.Fatalf("preview read environment variable %q", key)
		}

		return "tack-qa-preview"
	}
	err := Run(t.Context(), []string{"suburban/tack-qa"}, getenv, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "promotion branch") {
		t.Fatalf("expected mismatched deployment workspace refusal, got %v", err)
	}
}
