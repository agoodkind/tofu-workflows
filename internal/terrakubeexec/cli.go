package terrakubeexec

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// Main executes a deployment from Terrakube and returns its process status.
//
// Terrakube passes the workspace selector and collects the console output.
//
//	terrakube-configs vault/tack-prod
func Main(arguments []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := Run(ctx, arguments[1:], os.Environ(), os.Stdout, os.Stderr); err != nil {
		slog.Error("Configs deployment failed", "err", err)

		return 1
	}

	return 0
}
