package terrakubequeue

import (
	"errors"
	"slices"
	"testing"
)

func TestEnrollmentRejectsOverlappingDeploymentAuthority(t *testing.T) {
	enabled := true
	makeTarget := func(name string) Target {
		return Target{
			Name:        name,
			Root:        "opentofu/" + name,
			Branch:      "terrakube/" + name,
			Environment: "terrakube-" + name,
			Enabled:     &enabled,
			Shared:      []string{"common"},
		}
	}
	first := makeTarget("first")

	for _, tc := range []struct {
		name  string
		alter func(*application)
	}{
		{"repository", func(app *application) { app.Repository = "ORG/FIRST" }},
		{
			"environment",
			func(app *application) { app.Deployments[0].Environment = first.Environment },
		},
		{"promotion branch", func(app *application) { app.Deployments[0].Branch = first.Branch }},
		{"nested root", func(app *application) { app.Deployments[0].Root = first.Root + "/nested" }},
		{
			"outside repository",
			func(app *application) { app.Deployments[0].Root = "opentofu/../different" },
		},
		{
			"cross target input",
			func(app *application) { app.Deployments[0].Shared = []string{first.Root + "/files"} },
		},
	} {
		t.Run(
			tc.name,
			func(t *testing.T) {
				catalog := registry{
					Version: 1,
					Apps: []application{
						{Name: "first", Repository: "org/first", Deployments: []Target{first}},
						{
							Name:        "second",
							Repository:  "org/second",
							Deployments: []Target{makeTarget("second")},
						},
					},
				}
				entries, err := catalog.targets()
				if err != nil || len(entries) != 2 ||
					!slices.Equal(entries[1].Shared, []string{"common"}) {
					t.Fatalf("independent entries: %v", err)
				}

				tc.alter(&catalog.Apps[1])
				if _, err := catalog.targets(); !errors.Is(err, errRegistry) {
					t.Fatalf("accepted conflicting entry: %v", err)
				}
			},
		)
	}
}
