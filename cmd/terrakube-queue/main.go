// Command terrakube-queue submits reviewed Configs commits.
package main

import (
	"log/slog"
	"os"

	"github.com/agoodkind/tofu-workflows/internal/terrakubequeue"
)

func main() {
	slog.Debug("starting Configs queue")
	os.Exit(terrakubequeue.Main())
}
