// Package app connects the Terrakube queue to its command-line runtime.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agoodkind/tofu-workflows/internal/terrakubequeue"
	"github.com/agoodkind/tofu-workflows/internal/terrakubequeue/command"
	"github.com/sethvargo/go-githubactions"
	"github.com/spf13/cobra"
)

const (
	jobPollInterval = 15 * time.Second
	requestTimeout  = 30 * time.Second
)

// Main runs the Terrakube queue and returns its process exit status.
//
// It passes interrupt and termination signals to the running command.
//
// For example, list reviewed targets with the matrix command.
//
//	terrakube-queue matrix ./deployment preview
func Main() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newQueueCommand().ExecuteContext(ctx); err != nil {
		slog.Error("Terrakube queue failed", "error", err)

		return 1
	}

	return 0
}

func newQueueCommand() *cobra.Command {
	client := &http.Client{
		Timeout:       requestTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	service := terrakubequeue.NewService(terrakubequeue.Dependencies{
		Environment: os.Getenv,
		Token:       githubactions.GetIDToken,
		HTTPClient:  client,
		Wait:        waitForJob,
	})

	return command.New(service)
}

func waitForJob(ctx context.Context) error {
	timer := time.NewTimer(jobPollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		slog.WarnContext(ctx, "queue worker stopped polling", "err", ctx.Err())

		return fmt.Errorf("poll Terrakube job: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}
