// Package terrakubequeue deploys reviewed changes in merge order.
// Each workspace tracks the last commit claimed for deployment on a protected
// Git branch.
package terrakubequeue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

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

// sourceHistory reads reviewed commits and claims each promotion before job
// submission. A claim succeeds only if the promotion branch still points to
// the expected commit.
type sourceHistory interface {
	promotion(ctx context.Context) (string, error)
	nextCommit(ctx context.Context, current string) (string, error)
	promote(ctx context.Context, current, next string) error
}

var errReconcileApply = errors.New(
	"deployment may have started applying changes; reconcile workspace state before advancing",
)

var errQueue = errors.New("queue reconciliation failed")

type jobStatus string

// jobRecord records a Terrakube job's status and the commit reported by its
// executor. Before advancing, the queue checks that commit against the
// workspace's promotion branch.
type jobRecord struct {
	ID     string
	Status jobStatus
	Commit string
}

type jobService interface {
	lookup(ctx context.Context, sha string) ([]jobRecord, error)
	submit(ctx context.Context, sha string) (string, error)
	verifyFailedPlan(ctx context.Context, job jobRecord) error
}

// wake advances a single workspace in merge order. It checks the previous job
// before submitting another. For example, if the previous job is still running
// and a commit is waiting, wake tells the caller to check again later.
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
		case statusCancelled, statusRejected:
			return false, errReconcileApply
		case statusCompleted, statusNoChanges, statusFailed:
			if err := verifyTerminal(ctx, jobs, job, current); err != nil {
				return false, err
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

func verifyTerminal(ctx context.Context, jobs jobService, job jobRecord, current string) error {
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
			errQueue,
		)

		return fmt.Errorf(
			"%w: job %s finished at %q, expected %s",
			errQueue,
			job.ID,
			job.Commit,
			current,
		)
	}

	if job.Status == statusFailed {
		return jobs.verifyFailedPlan(ctx, job)
	}

	return nil
}
