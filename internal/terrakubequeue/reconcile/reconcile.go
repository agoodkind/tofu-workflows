// Package reconcile advances one deployment workspace in merge order.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// Job states control when the queue can advance.
const (
	StatusPending         Status = "pending"
	StatusQueue           Status = "queue"
	StatusRunning         Status = "running"
	StatusWaitingApproval Status = "waitingApproval"
	StatusApproved        Status = "approved"
	StatusCompleted       Status = "completed"
	StatusNoChanges       Status = "noChanges"
	StatusFailed          Status = "failed"
	StatusCancelled       Status = "cancelled"
	StatusRejected        Status = "rejected"
)

// Source reads reviewed commits and claims each promotion before job
// submission. A claim succeeds only if the promotion branch still points to
// the expected commit.
type Source interface {
	Promotion(ctx context.Context) (string, error)
	NextCommit(ctx context.Context, current string) (string, error)
	Promote(ctx context.Context, current, next string) error
}

// ErrReconcileApply requires an operator to check the workspace before the
// queue advances.
var ErrReconcileApply = errors.New(
	"deployment may have started applying changes; reconcile workspace state before advancing",
)

// ErrQueue indicates that the queue could not reconcile a deployment.
var ErrQueue = errors.New("queue reconciliation failed")

// Status is the state reported for a deployment job.
type Status string

// Job records a deployment job's status and the commit reported by its
// executor. Before advancing, the queue checks that commit against the
// workspace's promotion branch.
type Job struct {
	ID     string
	Status Status
	Commit string
}

// Jobs reads deployment jobs, submits them, and verifies failed plans.
type Jobs interface {
	Lookup(ctx context.Context, sha string) ([]Job, error)
	Submit(ctx context.Context, sha string) (string, error)
	VerifyFailedPlan(ctx context.Context, job Job) error
}

// Step checks the previous job before submitting the next commit.
//
// It reports whether any reviewed commit remains after the check.
//
//   - A running job keeps the next commit waiting.
//   - A completed job lets the queue submit the next commit.
func Step(
	ctx context.Context,
	baseline string,
	source Source,
	jobs Jobs,
) (bool, error) {
	current, err := source.Promotion(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "read promotion branch", "err", err)

		return false, fmt.Errorf("read promotion branch: %w", err)
	}

	if current != baseline {
		matches, err := jobs.Lookup(ctx, current)
		if err != nil {
			slog.ErrorContext(ctx, "look up promotion job", "commit", current, "err", err)

			return false, fmt.Errorf("look up job for promotion %s: %w", current, err)
		}

		if len(matches) != 1 {
			return false, fmt.Errorf(
				"%w: promotion %s has %d matching jobs; reconcile the existing submission",
				ErrQueue,
				current,
				len(matches),
			)
		}

		job := matches[0]
		switch job.Status {
		case StatusPending, StatusQueue, StatusRunning, StatusWaitingApproval, StatusApproved:
			next, err := findNextCommit(ctx, source, current)

			return next != "", err
		case StatusCancelled, StatusRejected:
			return false, ErrReconcileApply
		case StatusCompleted, StatusNoChanges, StatusFailed:
			if err := verifyTerminal(ctx, jobs, job, current); err != nil {
				return false, err
			}
		default:
			return false, fmt.Errorf(
				"%w: job %s for %s has status %q; inspect the job",
				ErrQueue,
				job.ID,
				current,
				job.Status,
			)
		}
	}

	next, err := findNextCommit(ctx, source, current)
	if err != nil || next == "" {
		return false, err
	}

	if err := source.Promote(ctx, current, next); err != nil {
		slog.ErrorContext(ctx, "advance promotion branch", "commit", next, "err", err)

		return false, fmt.Errorf("advance promotion branch to %s: %w", next, err)
	}

	jobID, err := jobs.Submit(ctx, next)
	if err != nil {
		slog.ErrorContext(ctx, "job submission failed", "commit", next, "error", err)

		return false, fmt.Errorf("promotion %s requires job lookup before retry: %w", next, err)
	}

	if jobID == "" {
		return false, fmt.Errorf("%w: Terrakube did not return a job ID for %s", ErrQueue, next)
	}

	slog.InfoContext(ctx, "submitted Terrakube job", "job", jobID, "commit", next)
	following, err := findNextCommit(ctx, source, next)

	return following != "", err
}

func findNextCommit(ctx context.Context, source Source, current string) (string, error) {
	next, err := source.NextCommit(ctx, current)
	if err != nil {
		slog.ErrorContext(ctx, "read next reviewed commit", "commit", current, "err", err)

		return next, fmt.Errorf("read next reviewed commit after %s: %w", current, err)
	}

	return next, nil
}

func verifyTerminal(ctx context.Context, jobs Jobs, job Job, current string) error {
	if job.Commit != current {
		slog.ErrorContext(
			ctx,
			"unexpected executor commit",
			"job",
			job.ID,
			"expected",
			current,
			"actual",
			job.Commit,
			"err",
			ErrQueue,
		)

		return fmt.Errorf(
			"%w: job %s finished at %q, expected %s",
			ErrQueue,
			job.ID,
			job.Commit,
			current,
		)
	}

	if job.Status == StatusFailed {
		if err := jobs.VerifyFailedPlan(ctx, job); err != nil {
			slog.ErrorContext(ctx, "verify failed plan", "job", job.ID, "err", err)

			return fmt.Errorf("verify failed plan for job %s: %w", job.ID, err)
		}
	}

	return nil
}

// Run advances a single deployment queue in merge order.
//
// It calls wait before checking again if another reviewed commit remains.
//
//   - When the queue catches up, Run returns nil.
//   - When a check or wait fails, Run returns the error.
func Run(
	ctx context.Context,
	baseline string,
	source Source,
	jobs Jobs,
	wait func(context.Context) error,
) error {
	for {
		pending, err := Step(ctx, baseline, source, jobs)
		if err != nil || !pending {
			return err
		}

		if err := wait(ctx); err != nil {
			return err
		}
	}
}
