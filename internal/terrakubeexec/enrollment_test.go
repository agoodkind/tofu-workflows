package terrakubeexec

import (
	"context"
	"io"
	"slices"
	"testing"
)

func TestEnrollmentSelectsADeploymentFromTheVerifiedCommit(t *testing.T) {
	const commit = "1234567890123456789012345678901234567890"
	const catalog = `{
		"version":1,
		"apps":[{
			"name":"search",
			"repository":"agoodkind/search",
			"deployments":[{
				"name":"search-qa",
				"root":"opentofu/suburban/search-qa",
				"promotion_branch":"terrakube/search-qa",
				"environment":"terrakube-search-qa",
				"shared_inputs":[]
			}]
		}]
	}`

	for _, test := range []struct {
		name      string
		workspace string
		branch    string
		wantError bool
	}{
		{name: "enrolled workspace", workspace: "search-qa-deploy", branch: "terrakube/search-qa"},
		{
			name:      "different workspace",
			workspace: "search-prod-deploy",
			branch:    "terrakube/search-qa",
			wantError: true,
		},
		{
			name:      "different branch",
			workspace: "search-qa-deploy",
			branch:    "terrakube/search-prod",
			wantError: true,
		},
	} {
		t.Run(
			test.name,
			func(t *testing.T) {
				git := func(_ context.Context, invocation command) error {
					if !slices.Equal(
						invocation.args,
						[]string{"show", commit + ":" + enrollmentFile},
					) {
						t.Fatalf(
							"expected the enrollment at the verified commit, got %v",
							invocation.args,
						)
					}

					_, err := io.WriteString(invocation.output, catalog)

					return err
				}
				selected := deployment{
					selector:  "suburban/search-qa",
					workspace: test.workspace,
					branch:    test.branch,
				}
				if err := requireEnrollment(
					t.Context(),
					git,
					commit,
					selected,
				); (err != nil) != test.wantError {
					t.Fatalf(
						"requireEnrollment returned %v, expected error = %v",
						err,
						test.wantError,
					)
				}
			},
		)
	}
}
