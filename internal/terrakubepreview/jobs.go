// Package terrakubepreview reports Configs pull request plans.
package terrakubepreview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var uuid = regexp.MustCompile("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")

const jobType = "job"

type jobState string

const (
	completed    jobState = "completed"
	pending      jobState = "pending"
	queued       jobState = "queue"
	running      jobState = "running"
	pollInterval          = 10 * time.Second
)

type envelope[T job | []job | []step] struct {
	Data T `json:"data"`
}

var (
	errMissing   = errors.New("the dispatcher has not submitted this preview")
	errStep      = errors.New("the completed job lacks a successful plan step and saved plan")
	errRequest   = errors.New("preview API request failed")
	errAmbiguous = errors.New("multiple Terrakube jobs match this preview")
	errJobID     = errors.New("invalid Terrakube job ID")
	errBinding   = errors.New("unexpected Terrakube job, workspace, template, or commit")
	errStatus    = errors.New("unsuccessful Terrakube plan")
)

type identity struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}
type step struct {
	identity

	Attributes struct {
		Number int      `json:"stepNumber"`
		Name   string   `json:"name"`
		Status jobState `json:"status"`
	} `json:"attributes"`
	Relationships struct {
		Job struct {
			Data identity `json:"data"`
		} `json:"job"`
	} `json:"relationships"`
}

const (
	planStepNumber = 100
	planStepName   = "Configs preview plan"
)

type job struct {
	identity

	Attributes struct {
		Plan     string   `json:"terraformPlan,omitempty"`
		SHA      string   `json:"commitId"`
		Marker   string   `json:"comments"`
		Template string   `json:"templateReference"`
		Status   jobState `json:"status,omitempty"`
	} `json:"attributes"`
	Relationships struct {
		Workspace struct {
			Data identity `json:"data"`
		} `json:"workspace"`
	} `json:"relationships"`
}
type api struct {
	base, org, template, token string
	client                     *http.Client
}

func request[T job | []job | []step](
	ctx context.Context,
	client api,
	method string,
	path string,
	payload []byte,
	output *envelope[T],
) error {
	req, err := http.NewRequestWithContext(
		ctx,
		method,
		client.base+"/organization/"+client.org+path,
		bytes.NewReader(payload),
	)
	if err != nil {
		slog.Error("prepare Terrakube request failed", "err", err)

		return fmt.Errorf("prepare Terrakube request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+client.token)
	req.Header.Set("Accept", "application/vnd.api+json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/vnd.api+json")
	}

	res, err := client.client.Do(req)
	if err != nil {
		return fmt.Errorf("send Terrakube request: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("%w: %s returned %s", errRequest, method, res.Status)
	}

	if err := json.NewDecoder(res.Body).Decode(output); err != nil {
		return fmt.Errorf("decode Terrakube response: %w", err)
	}

	return nil
}

func (client api) plan(
	ctx context.Context,
	workspace string,
	sha string,
	marker string,
	current func() error,
	wait func(context.Context) error,
) error {
	var wanted job
	wanted.Type = jobType
	wanted.Attributes.SHA = sha
	wanted.Attributes.Template = client.template
	wanted.Attributes.Marker = marker
	wanted.Relationships.Workspace.Data = identity{Type: "workspace", ID: workspace}
	query := url.Values{
		"filter[job]": {
			"workspace.id==" + workspace + ";comments==" + marker + ";templateReference==" + client.template,
		},
	}
	actual, err := client.findOrSubmit(ctx, query, wanted, current)
	if err != nil {
		return err
	}

	jobID := actual.ID
	if jobID == "" || strings.ContainsAny(jobID, "/?.#%") {
		return errJobID
	}

	for {
		if actual.ID != jobID || actual.Type != jobType || actual.Attributes.SHA != sha ||
			actual.Attributes.Template != client.template ||
			actual.Attributes.Marker != marker ||
			actual.Relationships.Workspace.Data != wanted.Relationships.Workspace.Data {
			return errBinding
		}

		switch actual.Attributes.Status {
		case completed:
			if err := client.confirmPlanStep(ctx, actual); err != nil {
				return err
			}

			verified, err := client.lookup(ctx, query)
			if err != nil {
				return err
			}

			if verified != actual {
				return errBinding
			}

			return current()
		case pending, queued, running:
		default:
			slog.Error(
				"Plan did not complete",
				"err",
				errStatus,
				"status",
				actual.Attributes.Status,
			)

			return fmt.Errorf("%w: %q", errStatus, actual.Attributes.Status)
		}

		if err := current(); err != nil {
			return err
		}

		if err := wait(ctx); err != nil {
			return err
		}

		actual, err = client.lookup(ctx, query)
		if err != nil {
			return err
		}
	}
}

func waitForPoll(ctx context.Context) error {
	timer := time.NewTimer(pollInterval)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		slog.Error("wait for preview failed", "err", ctx.Err())

		return fmt.Errorf("wait for preview: %w", ctx.Err())
	}
}

func (client api) confirmPlanStep(ctx context.Context, actual job) error {
	jobID := actual.ID
	if strings.TrimSpace(actual.Attributes.Plan) == "" {
		return errStep
	}

	var steps envelope[[]step]
	if err := request(ctx, client, http.MethodGet, "/job/"+jobID+"/step", nil, &steps); err != nil {
		return err
	}

	if len(steps.Data) != 1 {
		return errStep
	}

	evidence := steps.Data[0]
	if evidence.Type != "step" || evidence.ID == "" ||
		evidence.Attributes.Number != planStepNumber ||
		evidence.Attributes.Name != planStepName ||
		evidence.Attributes.Status != completed ||
		evidence.Relationships.Job.Data != (identity{Type: jobType, ID: jobID}) {
		return errStep
	}

	return nil
}

func (client api) lookup(ctx context.Context, query url.Values) (job, error) {
	var records envelope[[]job]
	if err := request(
		ctx,
		client,
		http.MethodGet,
		"/job?"+query.Encode(),
		nil,
		&records,
	); err != nil {
		return job{}, err
	}

	switch len(records.Data) {
	case 0:
		return job{}, errMissing
	case 1:
		return records.Data[0], nil
	default:
		return job{}, errAmbiguous
	}
}

// The dispatcher serializes each job marker across runners. An ambiguous POST
// ends this invocation. A later invocation must look up the marker again.
func (client api) findOrSubmit(
	ctx context.Context,
	query url.Values,
	wanted job,
	current func() error,
) (job, error) {
	actual, err := client.lookup(ctx, query)
	if errors.Is(err, errMissing) {
		if err := current(); err != nil {
			return job{}, err
		}

		payload, encodeErr := json.Marshal(envelope[job]{Data: wanted})
		if encodeErr != nil {
			slog.Error("Could not encode preview job", "err", encodeErr)

			return job{}, fmt.Errorf("encode preview job: %w", encodeErr)
		}

		var response envelope[job]
		if err := request(ctx, client, http.MethodPost, "/job", payload, &response); err != nil {
			return job{}, err
		}

		actual = response.Data
	} else if err != nil {
		return job{}, err
	}

	return actual, nil
}
