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
func TestTLSOriginRequiresExactServiceIdentity(t *testing.T) {
	binary := cloudflared(t)
	for _, test := range []struct {
		name, prefix string
		allowed      bool
	}{{"authorized identity", "CF_PROOF_", true}, {"valid but unauthorized identity", "CF_PROOF_WRONG_", false}, {"missing identity", "", false}} {
		t.Run(test.name, func(t *testing.T) {
			address := tunnel(t, binary, "deployment-qa-api-proof.goodkind.io", test.prefix)
			transport := &http.Transport{DialContext: func(ctx context.Context, network, target string) (net.Conn, error) {
				if target != "hypervisor.suburban.goodkind.io:8006" {
					return nil, fmt.Errorf("Unexpected fixed origin target")
				}
				return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, address)
			}}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://hypervisor.suburban.goodkind.io:8006/", nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(request)
			if !test.allowed {
				if err == nil {
					response.Body.Close()
					t.Fatal("An unauthorized Access identity established origin TLS")
				}
				t.Log("The origin TLS connection failed without an authorized Access identity.")
				return
			}
			if err != nil {
				t.Fatal("The authorized identity could not establish verified origin TLS")
			}
			defer response.Body.Close()
			body, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
			if err != nil || response.StatusCode != 200 || !bytes.Contains(body, []byte("Proxmox Virtual Environment")) {
				t.Fatal("The verified TLS response did not match the PVE login page")
			}
			if response.TLS == nil || len(response.TLS.VerifiedChains) == 0 {
				t.Fatal("The client did not verify the origin certificate")
			}
			t.Log("The default Go HTTP client verified the PVE certificate and read its public login page through the scoped TCP connection.")
		})
	}
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

func tunnel(t *testing.T, binary, hostname, prefix string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	command := exec.CommandContext(t.Context(), binary, "access", "tcp", "--hostname", hostname, "--url", address, "--log-level", "error")
	command.Env = os.Environ()
	if prefix != "" {
		for name, source := range map[string]string{"TUNNEL_SERVICE_TOKEN_ID": "CLIENT_ID", "TUNNEL_SERVICE_TOKEN_SECRET": "CLIENT_SECRET"} {
			command.Env = append(command.Env, name+"="+required(t, prefix+source))
		}
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
	return address
}

func sshProbe(t *testing.T, binary, prefix string) string {
	t.Helper()
	address := tunnel(t, binary, sshHostname, prefix)
	_, port, _ := net.SplitHostPort(address)
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
