package reconcile

import (
	"context"
	"errors"
	"testing"
)

type state struct {
	current, next       string
	jobs                []Job
	posts               int
	planFailureVerified bool
	err                 error
}

func (s *state) Promotion(ctx context.Context) (string, error) { return s.current, nil }

func (s *state) NextCommit(
	ctx context.Context,
	current string,
) (string, error) {
	if current == s.next {
		return "", nil
	}

	return s.next, nil
}

func (s *state) Promote(_ context.Context, old, next string) error {
	if s.current != old {
		return errors.New("ref changed")
	}

	s.current = next

	return nil
}
func (s *state) Lookup(ctx context.Context, sha string) ([]Job, error) { return s.jobs, nil }
func (s *state) VerifyFailedPlan(context.Context, Job) error {
	if !s.planFailureVerified {
		return ErrReconcileApply
	}

	return nil
}

func (s *state) Submit(ctx context.Context, sha string) (string, error) {
	s.posts++

	return "19", s.err
}

func TestLostSubmissionResponseNeverReposts(t *testing.T) {
	ctx := t.Context()
	s := &state{current: "initial", next: "first", err: errors.New("connection closed")}
	if _, err := Step(ctx, "initial", s, s); err == nil {
		t.Fatal("lost response needs reconciliation")
	}

	s.next = "second"
	if _, err := Step(ctx, "initial", s, s); err == nil {
		t.Fatal("unconfirmed submission must halt")
	}

	s.jobs = []Job{{ID: "19", Status: StatusRunning}}
	if _, err := Step(ctx, "initial", s, s); err != nil {
		t.Fatal(err)
	}

	if s.posts != 1 || s.current != "first" {
		t.Fatal(s)
	}

	s.jobs[0].Status = StatusFailed
	s.jobs[0].Commit = "other"
	if _, err := Step(ctx, "initial", s, s); err == nil {
		t.Fatal("wrong commit must halt")
	}

	s.jobs[0].Commit = "first"
	if _, err := Step(ctx, "initial", s, s); !errors.Is(err, ErrReconcileApply) {
		t.Fatal("failed apply must block promotion", err)
	}

	s.planFailureVerified = true
	s.err = nil
	if _, err := Step(ctx, "initial", s, s); err != nil {
		t.Fatal(err)
	}

	if s.posts != 2 || s.current != "second" {
		t.Fatal(s)
	}
}

func TestDuplicateRecoveryStopsOneWorkspace(t *testing.T) {
	qa := &state{current: "first", next: "second", jobs: []Job{{ID: "1"}, {ID: "2"}}}
	prod := &state{current: "base", next: "release"}
	if _, err := Step(t.Context(), "base", qa, qa); err == nil {
		t.Fatal("duplicate jobs must halt QA")
	}

	if _, err := Step(t.Context(), "base", prod, prod); err != nil {
		t.Fatal(err)
	}

	if qa.posts != 0 || prod.posts != 1 {
		t.Fatalf("qa %d, prod %d", qa.posts, prod.posts)
	}
}

func TestRunSubmitsNextCommitWhenRunningJobFinishes(t *testing.T) {
	s := &state{
		current: "first",
		next:    "second",
		jobs:    []Job{{ID: "1", Status: StatusRunning}},
	}
	polls := 0
	wait := func(context.Context) error {
		polls++
		s.jobs[0].Status = StatusCompleted
		s.jobs[0].Commit = "first"

		return nil
	}
	if err := Run(t.Context(), "base", s, s, wait); err != nil {
		t.Fatal(err)
	}

	if polls != 1 || s.posts != 1 || s.current != "second" {
		t.Fatalf("polls %d, submissions %d, commit %s", polls, s.posts, s.current)
	}
}
