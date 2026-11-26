package terrakubequeue

import (
	"context"
	"errors"
	"testing"
)

type state struct {
	current, next       string
	jobs                []jobRecord
	posts               int
	planFailureVerified bool
	err                 error
}

func (s *state) promotion(ctx context.Context) (string, error) { return s.current, nil }

func (s *state) nextCommit(
	ctx context.Context,
	current string,
) (string, error) {
	if current == s.next {
		return "", nil
	}

	return s.next, nil
}

func (s *state) promote(_ context.Context, old, next string) error {
	if s.current != old {
		return errors.New("ref changed")
	}

	s.current = next

	return nil
}
func (s *state) lookup(ctx context.Context, sha string) ([]jobRecord, error) { return s.jobs, nil }
func (s *state) verifyFailedPlan(context.Context, jobRecord) error {
	if !s.planFailureVerified {
		return errReconcileApply
	}

	return nil
}

func (s *state) submit(ctx context.Context, sha string) (string, error) {
	s.posts++

	return "19", s.err
}

func TestLostSubmissionResponseNeverReposts(t *testing.T) {
	ctx := t.Context()
	s := &state{current: "initial", next: "first", err: errors.New("connection closed")}
	if _, err := wake(ctx, "initial", s, s); err == nil {
		t.Fatal("lost response needs reconciliation")
	}

	s.next = "second"
	if _, err := wake(ctx, "initial", s, s); err == nil {
		t.Fatal("unconfirmed submission must halt")
	}

	s.jobs = []jobRecord{{ID: "19", Status: "running"}}
	if _, err := wake(ctx, "initial", s, s); err != nil {
		t.Fatal(err)
	}

	if s.posts != 1 || s.current != "first" {
		t.Fatal(s)
	}

	s.jobs[0].Status = "failed"
	s.jobs[0].Commit = "other"
	if _, err := wake(ctx, "initial", s, s); err == nil {
		t.Fatal("wrong commit must halt")
	}

	s.jobs[0].Commit = "first"
	if _, err := wake(ctx, "initial", s, s); !errors.Is(err, errReconcileApply) {
		t.Fatal("failed apply must block promotion", err)
	}

	s.planFailureVerified = true
	s.err = nil
	if _, err := wake(ctx, "initial", s, s); err != nil {
		t.Fatal(err)
	}

	if s.posts != 2 || s.current != "second" {
		t.Fatal(s)
	}
}

func TestDuplicateRecoveryStopsOneWorkspace(t *testing.T) {
	qa := &state{current: "first", next: "second", jobs: []jobRecord{{ID: "1"}, {ID: "2"}}}
	prod := &state{current: "base", next: "release"}
	if _, err := wake(t.Context(), "base", qa, qa); err == nil {
		t.Fatal("duplicate jobs must halt QA")
	}

	if _, err := wake(t.Context(), "base", prod, prod); err != nil {
		t.Fatal(err)
	}

	if qa.posts != 0 || prod.posts != 1 {
		t.Fatalf("qa %d, prod %d", qa.posts, prod.posts)
	}
}

func TestDrainSubmitsNextCommitWhenRunningJobFinishes(t *testing.T) {
	s := &state{
		current: "first",
		next:    "second",
		jobs:    []jobRecord{{ID: "1", Status: statusRunning}},
	}
	polls := 0
	wait := func(context.Context) error {
		polls++
		s.jobs[0].Status = statusCompleted
		s.jobs[0].Commit = "first"

		return nil
	}
	if err := drain(t.Context(), "base", s, s, wait); err != nil {
		t.Fatal(err)
	}

	if polls != 1 || s.posts != 1 || s.current != "second" {
		t.Fatalf("polls %d, submissions %d, commit %s", polls, s.posts, s.current)
	}
}
