package terrakubequeue

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	strictjson "sigs.k8s.io/json"
)

const (
	receiptPrefix        = "CONFIGS_TERRAKUBE_DEPLOYMENT_RESULT "
	receiptMaxLineLength = 4 << 20
)

type deploymentReceipt struct {
	Version     int    `json:"version"`
	JobID       string `json:"job_id"`
	WorkspaceID string `json:"workspace_id"`
	CommitID    string `json:"commit_id"`
	Outcome     string `json:"outcome"`
}

func (client apiClient) verifyFailedPlan(ctx context.Context, job jobRecord) error {
	number, err := strconv.ParseUint(job.ID, 10, 64)
	if err != nil || number == 0 || strconv.FormatUint(number, 10) != job.ID {
		return errUnexpectedJob
	}

	var steps envelope[[]resource]
	if err := request(
		ctx,
		client,
		http.MethodGet,
		"/job/"+job.ID+"/step",
		nil,
		&steps,
	); err != nil {
		return err
	}

	if len(steps.Data) != 1 {
		return errReconcileApply
	}

	step := steps.Data[0]
	if step.Type != "step" || !isCanonicalUUID(step.ID) || step.Attributes.Status != statusFailed {
		return errReconcileApply
	}

	endpoint, err := url.Parse(client.BaseURL)
	if err != nil {
		return fmt.Errorf("parse job log origin: %w", err)
	}

	endpoint.Path = "/tfoutput/v1/organization/" + client.Organization + "/job/" + job.ID + "/step/" + step.ID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("prepare job log request: %w", err)
	}

	if err := client.authorize(ctx, req); err != nil {
		return err
	}

	res, err := client.Client.Do(req)
	if err != nil {
		slog.ErrorContext(ctx, "read deployment receipt", "job", job.ID, "err", err)

		return fmt.Errorf("read job log: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Range") != "" ||
		strings.EqualFold(res.Header.Get("X-Terrakube-Output-Incomplete"), "true") {
		return errReconcileApply
	}

	expected := deploymentReceipt{
		Version:     1,
		Outcome:     "plan_failed",
		JobID:       job.ID,
		WorkspaceID: client.Workspace,
		CommitID:    job.Commit,
	}

	return readReceipt(res.Body, expected)
}

func readReceipt(log io.Reader, expected deploymentReceipt) error {
	scanner := bufio.NewScanner(log)
	scanner.Buffer(nil, receiptMaxLineLength)
	found := false

	for scanner.Scan() {
		line := scanner.Text()
		payload, ok := strings.CutPrefix(line, receiptPrefix)
		if !ok {
			continue
		}

		if found {
			return errReconcileApply
		}

		var actual deploymentReceipt
		problems, err := strictjson.UnmarshalStrict([]byte(payload), &actual)
		if err != nil || len(problems) != 0 || actual != expected {
			return errReconcileApply
		}

		found = true
	}

	if err := scanner.Err(); err != nil {
		slog.Error("read complete deployment log", "err", err)

		return fmt.Errorf("read complete deployment log: %w", err)
	}

	if !found {
		return errReconcileApply
	}

	return nil
}
