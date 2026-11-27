// Command terrakube-queue submits reviewed deployment commits to Terrakube in
// merge order. Each workspace has its own deployment queue.
package main

import (
	"log/slog"
	"os"

	"github.com/agoodkind/tofu-workflows/internal/terrakubequeue/app"
)

func main() {
	slog.Debug("starting Terrakube queue")
	os.Exit(app.Main())
}
