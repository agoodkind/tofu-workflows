// Command terrakube-preview reports plan results for a Configs pull request.
package main

import (
	"log/slog"
	"os"

	"github.com/agoodkind/tofu-workflows/internal/terrakubepreview"
)

func main() {
	slog.Info("Checking Configs pull request plans")
	os.Exit(terrakubepreview.Main())
}
