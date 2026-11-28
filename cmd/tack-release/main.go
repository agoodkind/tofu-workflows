// Command tack-release publishes Tack release pins to Configs.
package main

import (
	"log/slog"
	"os"

	"github.com/agoodkind/tofu-workflows/internal/release/tack"
)

func main() {
	slog.Info("Start Tack release publisher")
	os.Exit(tack.Run())
}
