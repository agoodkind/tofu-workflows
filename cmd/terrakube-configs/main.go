// Command terrakube-configs deploys a promoted Configs revision.
package main

import (
	"log/slog"
	"os"

	"github.com/agoodkind/tofu-workflows/internal/terrakubeexec"
)

func main() {
	status := terrakubeexec.Main(os.Args)
	if status != 0 {
		slog.Debug("Terrakube deployment exited", "status", status)
		os.Exit(status)
	}
}
