// Package terrakubequeue reconciles protected Configs promotion branches.
package terrakubequeue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// sourceHistory reads trusted main and advances the promotion ref
// only if its previous value still matches the caller's observation.
type sourceHistory interface {
	promotion(ctx context.Context) (string, error)
	nextCommit(ctx context.Context, current string) (string, error)
	promote(ctx context.Context, current, next string) error
}

var errQueue = errors.New("queue reconciliation failed")

type jobStatus string

const (
	statusPending         jobStatus = "pending"
	statusQueue           jobStatus = "queue"
	statusRunning         jobStatus = "running"
	statusWaitingApproval jobStatus = "waitingApproval"
	statusApproved        jobStatus = "approved"
	statusCompleted       jobStatus = "completed"
	statusNoChanges       jobStatus = "noChanges"
	statusFailed          jobStatus = "failed"
	statusCancelled       jobStatus = "cancelled"
	statusRejected        jobStatus = "rejected"
)

// jobRecord identifies one Terrakube job and its executor checkout.
type jobRecord struct {
	ID     string
	Status jobStatus
	Commit string
}

type jobService interface {
	lookup(ctx context.Context, sha string) ([]jobRecord, error)
	submit(ctx context.Context, sha string) (string, error)
}

// wake submits at most one job and reports whether another commit is waiting.
func wake(
	ctx context.Context,
	baseline string,
	source sourceHistory,
	jobs jobService,
) (bool, error) {
	current, err := source.promotion(ctx)
	if err != nil {
		return false, err
	}

	if current != baseline {
		matches, err := jobs.lookup(ctx, current)
		if err != nil {
			return false, err
		}

		if len(matches) != 1 {
			return false, fmt.Errorf(
				"%w: promotion %s has %d matching jobs; reconcile the existing submission",
				errQueue,
				current,
				len(matches),
			)
		}

		job := matches[0]
		switch job.Status {
		case statusPending, statusQueue, statusRunning, statusWaitingApproval, statusApproved:
			next, err := source.nextCommit(ctx, current)

			return next != "", err
		case statusCompleted, statusNoChanges, statusFailed, statusCancelled, statusRejected:
			if job.Commit != current {
				return false, fmt.Errorf(
					"%w: job %s finished at %q, expected %s",
					errQueue,
					job.ID,
					job.Commit,
					current,
				)
			}
		default:
			return false, fmt.Errorf(
				"%w: job %s for %s has status %q; inspect the job",
				errQueue,
				job.ID,
				current,
				job.Status,
			)
		}
	}

	next, err := source.nextCommit(ctx, current)
	if err != nil || next == "" {
		return false, err
	}

	if err := source.promote(ctx, current, next); err != nil {
		return false, err
	}

	jobID, err := jobs.submit(ctx, next)
	if err != nil {
		slog.ErrorContext(ctx, "job submission failed", "commit", next, "error", err)

		return false, fmt.Errorf("promotion %s requires job lookup before retry: %w", next, err)
	}

	if jobID == "" {
		return false, fmt.Errorf("%w: Terrakube did not return a job ID for %s", errQueue, next)
	}

	slog.InfoContext(ctx, "submitted Terrakube job", "job", jobID, "commit", next)
	following, err := source.nextCommit(ctx, next)

	return following != "", err
}
