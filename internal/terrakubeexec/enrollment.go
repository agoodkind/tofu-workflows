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
	promotionBranch      = regexp.MustCompile(`^terrakube/[a-z0-9]+(-[a-z0-9]+)*$`)
	errEnrollmentVersion = errors.New("unsupported Configs enrollment version")
	errEnrollmentTarget  = errors.New("verified Configs enrollment does not match this deployment")
)

type enrolledDeployment struct {
	Name            string `json:"name"`
	Root            string `json:"root"`
	PromotionBranch string `json:"promotion_branch"`
}

type enrollment struct {
	Version int `json:"version"`
	Apps    []struct {
		Deployments []enrolledDeployment `json:"deployments"`
	} `json:"apps"`
}

func requireEnrollment(
	ctx context.Context,
	git commandRunner,
	commit string,
	selected deployment,
) error {
	raw, err := gitOutput(ctx, git, "show", commit+":"+enrollmentFile)
	if err != nil {
		return err
	}

	var catalog enrollment
	if err := json.Unmarshal([]byte(raw), &catalog); err != nil {
		return executionError("decode the verified Configs enrollment", err)
	}

	if catalog.Version != 1 {
		return errEnrollmentVersion
	}

	matches := 0

	for _, app := range catalog.Apps {
		for _, target := range app.Deployments {
			if target.Root != "opentofu/"+selected.selector {
				continue
			}

			if target.Name+"-deploy" != selected.workspace ||
				target.PromotionBranch != selected.branch {
				return errEnrollmentTarget
			}

			matches++
		}
	}

	if matches != 1 {
		return errEnrollmentTarget
	}

	return nil
}
