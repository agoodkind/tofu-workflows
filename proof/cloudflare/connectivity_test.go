package cloudflare_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const apiURL = "https://deployment-qa-api-proof.goodkind.io/"
const sshHostname = "deployment-qa-ssh-proof.goodkind.io"
const cloudflaredURL = "https://github.com/cloudflare/cloudflared/releases/download/2026.9.1/cloudflared-linux-amd64"
const cloudflaredDigest = "03f1f25d1cc93b9ad6c60569d44060bc4f17ed97075760ed8cfca4b12dcd68cc"
const trustedHostKey = "tack-qa-proof ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAKGIDBA5fCzrMH4dws9thYA+dX1n0Z1P1yfZJ0P2mX0\n"

func required(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("Required credential reference %s is empty", name)
	}
	return value
}
func accessRequest(t *testing.T, prefix string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, apiURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("User-Agent", "cloudflared/2026.9.1")
	if prefix != "" {
		request.Header.Set("CF-Access-Client-ID", required(t, prefix+"CLIENT_ID"))
		request.Header.Set("CF-Access-Client-Secret", required(t, prefix+"CLIENT_SECRET"))
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("HTTPS connection failed")
	}
	defer response.Body.Close()
	if response.Header.Get("CF-Mitigated") == "challenge" {
		t.Fatal("A browser challenge prevented the Access identity check")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
	if err != nil {
		t.Fatal("HTTPS response could not be read")
	}
	return response.StatusCode, body
}

func TestHTTPSRequiresExactServiceIdentity(t *testing.T) {
	for _, test := range []struct{ name, prefix string }{{"missing identity", ""}, {"valid but unauthorized identity", "CF_PROOF_WRONG_"}} {
		t.Run(test.name, func(t *testing.T) {
			status, _ := accessRequest(t, test.prefix)
			if status != http.StatusForbidden {
				t.Fatalf("Access returned status %d; expected403", status)
			}
			t.Logf("Access rejected the request with HTTP%d", status)
		})
	}
	t.Run("authorized identity", func(t *testing.T) {
		status, body := accessRequest(t, "CF_PROOF_")
		if status != http.StatusOK || !bytes.Contains(body, []byte("Proxmox Virtual Environment")) {
			t.Fatalf("Verified HTTPS origin response did not match the PVE login page; status=%d", status)
		}
		t.Log("The exact Access identity received the public PVE login page over verified HTTPS.")
	})
}

func cloudflared(t *testing.T) string {
	t.Helper()
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, cloudflaredURL, nil)
	response, err := (&http.Client{Timeout: time.Minute}).Do(request)
	if err != nil {
		t.Fatal("cloudflared download failed")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("cloudflared download status%d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 128*1024*1024))
	if err != nil {
		t.Fatal("cloudflared download read failed")
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != cloudflaredDigest {
		t.Fatal("cloudflared checksum differs from the pinned release")
	}
	path := filepath.Join(t.TempDir(), "cloudflared")
	if err := os.WriteFile(path, body, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func sshProbe(t *testing.T, binary, prefix string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_, port, _ := net.SplitHostPort(address)
	listener.Close()
	command := exec.CommandContext(t.Context(), binary, "access", "tcp", "--hostname", sshHostname, "--url", address, "--log-level", "error")
	command.Env = os.Environ()
	for name, source := range map[string]string{"TUNNEL_SERVICE_TOKEN_ID": "CLIENT_ID", "TUNNEL_SERVICE_TOKEN_SECRET": "CLIENT_SECRET"} {
		command.Env = append(command.Env, name+"="+required(t, prefix+source))
	}
	var diagnostics bytes.Buffer
	command.Stdout = &diagnostics
	command.Stderr = &diagnostics
	if err := command.Start(); err != nil {
		t.Fatal("cloudflared could not start")
	}
	t.Cleanup(func() { command.Process.Kill(); command.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			connection.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cloudflared did not open its local listener")
		}
		time.Sleep(50 * time.Millisecond)
	}
	hosts := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(hosts, []byte(trustedHostKey), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	ssh := exec.CommandContext(ctx, "ssh", "-F", "/dev/null", "-p", port, "-o", "HostKeyAlias=tack-qa-proof", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+hosts, "-o", "GlobalKnownHostsFile=/dev/null", "-o", "HostKeyAlgorithms=ssh-ed25519", "-o", "BatchMode=yes", "-o", "PreferredAuthentications=none", "-o", "ConnectTimeout=10", "deployment-readiness-proof@127.0.0.1", "true")
	output, err := ssh.CombinedOutput()
	if err == nil {
		t.Fatal("The SSH proof unexpectedly authenticated")
	}
	if ctx.Err() != nil {
		t.Fatal("SSH proof exceeded its deadline")
	}
	return string(output)
}

func TestSSHVerifiesHostKeyWithoutLoginCredentials(t *testing.T) {
	binary := cloudflared(t)
	t.Run("authorized identity", func(t *testing.T) {
		output := sshProbe(t, binary, "CF_PROOF_")
		if !strings.Contains(output, "Permission denied") || strings.Contains(output, "Host key verification failed") {
			t.Fatal("SSH did not confirm the pinned host key before rejecting authentication")
		}
		t.Log("SSH verified the existing guest host key and then rejected login without credentials.")
	})
	t.Run("unauthorized identity", func(t *testing.T) {
		output := sshProbe(t, binary, "CF_PROOF_WRONG_")
		if strings.Contains(output, "Permission denied") {
			t.Fatal("The unauthorized Access identity connected to the SSH authentication service")
		}
		if !strings.Contains(output, "Connection closed") && !strings.Contains(output, "Connection reset") {
			t.Fatal(fmt.Sprintf("SSH denial did not produce the expected connection rejection; bytes=%d", len(output)))
		}
		t.Log("The unauthorized Access identity could not connect to the guest SSH service.")
	})
}
