package githubapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v93/github"

	"github.com/agoodkind/tofu-workflows/internal/githubapi"
)

const (
	syntheticToken  = "synthetic-token-value-0123456789"
	apiPath         = "/api/v3/"
	uploadPath      = "/api/uploads/"
	zenPath         = "/api/v3/zen"
	zenBody         = "Keep it logically awesome."
	expectedTimeout = 30 * time.Second
)

func TestNewClientRejectsAnEmptyToken(t *testing.T) {
	client, err := githubapi.NewClient("")
	if err == nil {
		t.Fatal("NewClient accepted an empty token")
	}
	if client != nil {
		t.Fatalf("NewClient returned a client with an empty token: %v", client)
	}
}

func TestNewClientBoundsTheHTTPTimeout(t *testing.T) {
	client, err := githubapi.NewClient(syntheticToken)
	if err != nil {
		t.Fatal(err)
	}
	if got := client.Client().Timeout; got != expectedTimeout {
		t.Fatalf("HTTP timeout = %v, want %v", got, expectedTimeout)
	}
}

func TestNewClientSendsTheTokenToTheConfiguredOrigin(t *testing.T) {
	var gotAuthorization, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if _, err := w.Write([]byte(zenBody)); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	client := pointAt(t, server.URL)

	zen, _, err := client.Zen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if zen != zenBody {
		t.Fatalf("Zen = %q, want %q", zen, zenBody)
	}
	if gotPath != zenPath {
		t.Fatalf("request path = %q, want %q", gotPath, zenPath)
	}
	if want := "Bearer " + syntheticToken; gotAuthorization != want {
		t.Fatal("the request did not carry the bearer token")
	}
}

func TestNewClientErrorsOmitTheToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	client := pointAt(t, server.URL)

	_, _, err := client.Zen(context.Background())
	if err == nil {
		t.Fatal("Zen succeeded against a server that returned 401")
	}
	if strings.Contains(err.Error(), syntheticToken) {
		t.Fatal("the request error contains the token")
	}
}

// pointAt returns a client from the exported constructor that talks to the
// test server. Clone keeps the token and the timeout and scopes the token to
// the server origin.
func pointAt(t *testing.T, serverURL string) *github.Client {
	t.Helper()
	client, err := githubapi.NewClient(syntheticToken)
	if err != nil {
		t.Fatal(err)
	}
	local, err := client.Clone(github.WithEnterpriseURLs(serverURL+apiPath, serverURL+uploadPath))
	if err != nil {
		t.Fatal(err)
	}
	return local
}
