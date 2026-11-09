// Package githubapi creates authenticated GitHub API clients.
package githubapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/go-github/v93/github"
)

const requestTimeout = 30 * time.Second

var errEmptyToken = errors.New("the GitHub token is empty")

// NewClient creates an authenticated GitHub client with a bounded HTTP timeout.
func NewClient(token string) (*github.Client, error) {
	if token == "" {
		return nil, errEmptyToken
	}

	httpClient := &http.Client{Timeout: requestTimeout}
	client, err := github.NewClient(github.WithHTTPClient(httpClient), github.WithAuthToken(token))
	if err != nil {
		slog.Error("GitHub client creation failed", "err", err)
		return nil, fmt.Errorf("create the GitHub client: %w", err)
	}

	return client, nil
}
