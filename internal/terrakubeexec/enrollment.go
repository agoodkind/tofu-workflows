package terrakubeexec

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
)

const enrollmentFile = ".github/terrakube/enrollments.json"

var (
	workspaceSelector    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*(/[a-z0-9][a-z0-9_-]*)+$`)
	deploymentWorkspace  = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*-deploy$`)
	enrollmentName       = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	promotionBranch      = regexp.MustCompile(`^terrakube/[a-z0-9]+(-[a-z0-9]+)*$`)
	errEnrollmentVersion = errors.New("unsupported Configs enrollment version")
	errEnrollmentTarget  = errors.New("verified Configs enrollment does not match this deployment")
)

type enrolledDeployment struct {
	Name            string `json:"name"`
	Root            string `json:"root"`
	PromotionBranch string `json:"promotion_branch"`
	Enabled         bool   `json:"enabled"`
}

type enrollment struct {
	Version int `json:"version"`
	Apps    []struct {
		Name        string               `json:"name"`
		Deployments []enrolledDeployment `json:"deployments"`
	} `json:"apps"`
}

type deploymentIdentity struct {
	service    string
	deployment string
}

func requireEnrollment(
	ctx context.Context,
	git commandRunner,
	commit string,
	selected deployment,
) (deploymentIdentity, error) {
	raw, err := gitOutput(ctx, git, "show", commit+":"+enrollmentFile)
	if err != nil {
		return deploymentIdentity{}, err
	}

	var catalog enrollment
	if err := json.Unmarshal([]byte(raw), &catalog); err != nil {
		return deploymentIdentity{}, executionError("decode the verified Configs enrollment", err)
	}

	if catalog.Version != 1 {
		return deploymentIdentity{}, errEnrollmentVersion
	}

	var identity deploymentIdentity

	for _, app := range catalog.Apps {
		for _, target := range app.Deployments {
			if target.Root != "opentofu/"+selected.selector {
				continue
			}

			if identity.service != "" || !target.Enabled || !enrollmentName.MatchString(app.Name) ||
				target.Name+"-deploy" != selected.workspace ||
				target.PromotionBranch != selected.branch {
				return deploymentIdentity{}, errEnrollmentTarget
			}

			identity = deploymentIdentity{service: app.Name, deployment: target.Name}
		}
	}

	if identity.service == "" {
		return deploymentIdentity{}, errEnrollmentTarget
	}

	return identity, nil
}
