package terrakubequeue

import (
	"bufio"
	"bytes"
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
	receiptVersion         = 1
	stepType               = "step"
	jobStepsPathFormat     = jobAPIPath + "/%s/step"
	stepOutputPathFormat   = "/tfoutput/v1/organization/%s/job/%s/step/%s"
	contentRangeHeader     = "Content-Range"
	outputIncompleteHeader = "X-Terrakube-Output-Incomplete"
	incompleteOutputValue  = "true"
	planFailedOutcome      = "plan_failed"
	receiptPrefix          = "TERRAKUBE_DEPLOYMENT_RESULT "
	receiptMaxLineLength   = 4 << 20
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
		fmt.Sprintf(jobStepsPathFormat, job.ID),
		nil,
		&steps,
	); err != nil {
		return err
	}

	if len(steps.Data) != 1 {
		return errReconcileApply
	}

	step := steps.Data[0]
	if step.Type != stepType || !isCanonicalUUID(step.ID) ||
		step.Attributes.Status != statusFailed {
		return errReconcileApply
	}

	endpoint, err := url.Parse(client.BaseURL)
	if err != nil {
		return fmt.Errorf("parse job log origin: %w", err)
	}

	endpoint.Path = fmt.Sprintf(stepOutputPathFormat, client.Organization, job.ID, step.ID)
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
	if res.StatusCode != http.StatusOK || res.Header.Get(contentRangeHeader) != "" ||
		strings.EqualFold(res.Header.Get(outputIncompleteHeader), incompleteOutputValue) {
		return errReconcileApply
	}

	expected := deploymentReceipt{
		Version:     receiptVersion,
		Outcome:     planFailedOutcome,
		JobID:       job.ID,
		WorkspaceID: client.Workspace,
		CommitID:    job.Commit,
	}

	return readReceipt(res.Body, expected)
}

// readReceipt checks the complete job log for one expected deployment result.
// For example, a second matching plan_failed record invalidates the log.
func readReceipt(log io.Reader, expected deploymentReceipt) error {
	scanner := bufio.NewScanner(log)
	scanner.Buffer(nil, receiptMaxLineLength)
	found := false

	for scanner.Scan() {
		payload, ok := bytes.CutPrefix(scanner.Bytes(), []byte(receiptPrefix))
		if !ok {
			continue
		}

		if found {
			return errReconcileApply
		}

		var actual deploymentReceipt
		problems, err := strictjson.UnmarshalStrict(payload, &actual)
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
