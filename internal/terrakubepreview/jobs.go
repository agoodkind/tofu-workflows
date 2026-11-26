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

type jobState string

const (
	completed    jobState = "completed"
	pending      jobState = "pending"
	queued       jobState = "queue"
	running      jobState = "running"
	pollInterval          = 10 * time.Second
)

type envelope[T job | []job] struct {
	Data T `json:"data"`
}

var (
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
type job struct {
	identity

	Attributes struct {
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

func request[T job | []job](
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
	wanted.Type = "job"
	wanted.Attributes.SHA = sha
	wanted.Attributes.Template = client.template
	wanted.Attributes.Marker = marker
	wanted.Relationships.Workspace.Data = identity{Type: "workspace", ID: workspace}
	query := url.Values{
		"filter[job]": {
			"workspace.id==" + workspace + ";comments==" + marker + ";templateReference==" + client.template,
		},
	}
	var results envelope[[]job]
	if err := request(ctx, client, "GET", "/job?"+query.Encode(), nil, &results); err != nil {
		return err
	}

	var record envelope[job]
	switch len(results.Data) {
	case 0:
		if err := current(); err != nil {
			return err
		}

		record.Data = wanted
		payload, err := json.Marshal(record)
		if err != nil {
			slog.Error("encode preview request failed", "err", err)

			return fmt.Errorf("encode preview request: %w", err)
		}

		record.Data = job{}
		if err := request(ctx, client, "POST", "/job", payload, &record); err != nil {
			return err
		}
	case 1:
		record.Data = results.Data[0]
	default:
		return errAmbiguous
	}

	jobID := record.Data.ID
	if jobID == "" || strings.ContainsAny(jobID, "/?.#%") {
		return errJobID
	}

	for {
		actual := record.Data
		if actual.ID != jobID || actual.Type != "job" || actual.Attributes.SHA != sha ||
			actual.Attributes.Template != client.template ||
			actual.Attributes.Marker != marker ||
			actual.Relationships.Workspace.Data != wanted.Relationships.Workspace.Data {
			return errBinding
		}

		switch actual.Attributes.Status {
		case completed:
			return current()
		case pending, queued, running:
		default:
			return fmt.Errorf("%w: %q", errStatus, actual.Attributes.Status)
		}

		if err := current(); err != nil {
			return err
		}

		if err := wait(ctx); err != nil {
			return err
		}

		record.Data = job{}
		if err := request(ctx, client, "GET", "/job/"+jobID, nil, &record); err != nil {
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
