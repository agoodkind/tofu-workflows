package r2buckets

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"
)

const qaBucket = "deployment-readiness-qa-20261023"
const prodBucket = "deployment-readiness-prod-20261023"
const endpoint = "https://ee7d7ca7d611ef8c2a07885e8362de0c.r2.cloudflarestorage.com"

type storage struct{ endpoint, access, secret, bucket string }

func assignment(name, value string) string { return fmt.Sprintf("%s=%s", name, value) }

func randomText(t *testing.T) string {
	t.Helper()
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(data)
}
func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func mac(key []byte, value string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(value))
	return h.Sum(nil)
}

func (s storage) request(method, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequest(method, s.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	now := time.Now().UTC()
	day := now.Format("20060102")
	stamp := now.Format("20060102T150405Z")
	payloadHash := digest(body)
	req.Header.Set("x-amz-date", stamp)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	headers := "host:" + req.URL.Host + "\nx-amz-content-sha256:" + payloadHash + "\nx-amz-date:" + stamp + "\n"
	signed := "host;x-amz-content-sha256;x-amz-date"
	canonical := method + "\n" + req.URL.EscapedPath() + "\n" + req.URL.Query().Encode() + "\n" + headers + "\n" + signed + "\n" + payloadHash
	scope := day + "/auto/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + digest([]byte(canonical))
	signingKey := mac(mac(mac(mac([]byte("AWS4"+s.secret), day), "auto"), "s3"), "aws4_request")
	signature := hex.EncodeToString(mac(signingKey, stringToSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 %s=%s/%s, SignedHeaders=%s, Signature=%s", "Credential", s.access, scope, signed, signature))
	client := http.Client{Timeout: 15 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	return response.StatusCode, data, err
}

func command(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	output, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v: %s", name, err, output)
	}
	return output
}

func tofu(dir string, environment []string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tofu", args...)
	cmd.Dir = dir
	cmd.Env = environment
	output, err := cmd.CombinedOutput()
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok && value != "" && (strings.HasPrefix(name, "AWS_") || strings.HasPrefix(name, "TF_VAR_") || strings.HasPrefix(name, "R2_BUCKET_PROOF_")) {
			output = bytes.ReplaceAll(output, []byte(value), []byte("[redacted]"))
		}
	}
	return output, err
}

func root(t *testing.T, base, name, key, bucket, endpoint string) string {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("main.tf")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), source, 0600); err != nil {
		t.Fatal(err)
	}
	tmpl, err := template.ParseFiles("backend.hcl.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	var backend bytes.Buffer
	inputs := struct{ Key, Endpoint, Bucket string }{key, endpoint, bucket}
	if err := tmpl.Execute(&backend, inputs); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "backend.hcl"), backend.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func client(t *testing.T, label, bucket string) storage {
	t.Helper()
	access := os.Getenv("R2_BUCKET_PROOF_" + label + "_ACCESS_KEY_ID")
	secret := os.Getenv("R2_BUCKET_PROOF_" + label + "_SECRET_ACCESS_KEY")
	if access == "" || secret == "" {
		t.Fatalf("Missing credential for %s", label)
	}
	return storage{endpoint, access, secret, bucket}
}
func childEnvironment(base []string, s storage, key, payload string) []string {
	return append(append([]string{}, base...), assignment("AWS_ACCESS_KEY_ID", s.access), assignment("AWS_SECRET_ACCESS_KEY", s.secret), "AWS_EC2_METADATA_DISABLED=true", assignment("TF_VAR_vault_tofu_state_passphrase", key), assignment("TF_VAR_payload", payload))
}
func requireStatus(t *testing.T, s storage, method, path string, body []byte, expected int) []byte {
	t.Helper()
	status, data, err := s.request(method, path, body)
	if err != nil || status != expected {
		t.Fatalf("%s %s: expected%d got%d; network error=%v", method, path, expected, status, err)
	}
	return data
}
func TestDirectBucketCredentials(t *testing.T) {
	qa := client(t, "QA", qaBucket)
	prod := client(t, "PROD", prodBucket)
	preview := client(t, "PREVIEW", qaBucket)
	key, payload := randomText(t), "synthetic-bucket-state-"+randomText(t)
	clean := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "AWS_") || strings.HasPrefix(name, "TF_") || strings.HasPrefix(name, "R2_BUCKET_PROOF_") {
			continue
		}
		clean = append(clean, entry)
	}
	qaEnv, prodEnv, previewEnv := childEnvironment(clean, qa, key, payload), childEnvironment(clean, prod, key, payload), childEnvironment(clean, preview, key, payload)
	base := t.TempDir()
	qaRoot := root(t, base, "qa", "tack-qa.tfstate", qaBucket, endpoint)
	prodRoot := root(t, base, "prod", "tack-prod.tfstate", prodBucket, endpoint)
	previewRoot := root(t, base, "preview", "tack-qa.tfstate", qaBucket, endpoint)
	t.Cleanup(func() {
		for _, entry := range []struct {
			client storage
			key    string
		}{{qa, "tack-qa.tfstate"}, {prod, "tack-prod.tfstate"}} {
			for _, suffix := range []string{"", ".tflock"} {
				status, _, err := entry.client.request("DELETE", "/"+entry.client.bucket+"/"+entry.key+suffix, nil)
				if err != nil || status != 204 {
					t.Errorf("Cleanup failed for synthetic object %s: status%d", entry.key+suffix, status)
				}
			}
		}
	})
	for _, entry := range []struct {
		client  storage
		sibling string
	}{{qa, prodBucket}, {prod, qaBucket}} {
		requireStatus(t, entry.client, "GET", "/", nil, 403)
		requireStatus(t, entry.client, "GET", "/"+entry.sibling+"?list-type=2", nil, 403)
		requireStatus(t, entry.client, "GET", "/"+entry.sibling+"/denied-synthetic", nil, 403)
		requireStatus(t, entry.client, "PUT", "/"+entry.sibling+"/denied-synthetic", []byte("synthetic"), 403)
		requireStatus(t, entry.client, "GET", "/"+entry.client.bucket+"?list-type=2", nil, 200)
	}
	t.Log("Direct bucket tokens allow own-bucket listing and deny sibling GET, PUT, listing, and global bucket listing")
	for _, entry := range []struct {
		dir string
		env []string
	}{{qaRoot, qaEnv}, {prodRoot, prodEnv}} {
		if output, err := tofu(entry.dir, entry.env, "init", "-backend-config=backend.hcl", "-input=false", "-no-color"); err != nil {
			t.Fatalf("Initialize encrypted root: %v: %s", err, output)
		}
		if output, err := tofu(entry.dir, entry.env, "plan", "-out=release.plan", "-input=false", "-no-color"); err != nil {
			t.Fatalf("Plan encrypted root: %v: %s", err, output)
		}
		plan, err := os.ReadFile(filepath.Join(entry.dir, "release.plan"))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(plan, []byte(payload)) || !bytes.Contains(plan, []byte("encrypted_data")) {
			t.Fatal("Saved plan does not satisfy encryption assertion")
		}
		if output, err := tofu(entry.dir, entry.env, "apply", "-input=false", "-no-color", "release.plan"); err != nil {
			t.Fatalf("Apply encrypted plan: %v: %s", err, output)
		}
	}
	qaState := requireStatus(t, qa, "GET", "/"+qaBucket+"/tack-qa.tfstate", nil, 200)
	prodState := requireStatus(t, prod, "GET", "/"+prodBucket+"/tack-prod.tfstate", nil, 200)
	for _, state := range [][]byte{qaState, prodState} {
		if bytes.Contains(state, []byte(payload)) || !bytes.Contains(state, []byte("encrypted_data")) {
			t.Fatal("Stored object does not satisfy encryption assertion")
		}
	}
	requireStatus(t, qa, "GET", "/"+prodBucket+"/tack-prod.tfstate", nil, 403)
	requireStatus(t, prod, "GET", "/"+qaBucket+"/tack-qa.tfstate", nil, 403)
	t.Log("OpenTofu applied encrypted saved plans into both buckets; each token also denies the sibling's existing state")
	if output, err := tofu(previewRoot, previewEnv, "init", "-backend-config=backend.hcl", "-input=false", "-no-color"); err != nil {
		t.Fatalf("Read-only preview init failed: %v: %s", err, output)
	}
	if output, err := tofu(previewRoot, previewEnv, "plan", "-lock=false", "-out=preview.plan", "-input=false", "-no-color"); err != nil {
		t.Fatalf("Read-only preview plan failed: %v: %s", err, output)
	}
	previewPlan, err := os.ReadFile(filepath.Join(previewRoot, "preview.plan"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(previewPlan, []byte("encrypted_data")) || bytes.Contains(previewPlan, []byte(payload)) {
		t.Fatal("Preview saved plan does not satisfy encryption assertion")
	}
	requireStatus(t, preview, "GET", "/"+qaBucket+"/tack-qa.tfstate", nil, 200)
	requireStatus(t, preview, "PUT", "/"+qaBucket+"/tack-qa.tfstate", []byte("synthetic-denied"), 403)
	requireStatus(t, preview, "DELETE", "/"+qaBucket+"/tack-qa.tfstate", nil, 403)
	requireStatus(t, preview, "PUT", "/"+qaBucket+"/tack-qa.tfstate.tflock", []byte("synthetic-denied"), 403)
	requireStatus(t, preview, "GET", "/"+prodBucket+"/tack-prod.tfstate", nil, 403)
	if !bytes.Equal(qaState, requireStatus(t, qa, "GET", "/"+qaBucket+"/tack-qa.tfstate", nil, 200)) {
		t.Fatal("Read-only preview changed QA state")
	}
	t.Log("Object-read preview token initializes and plans with lock=false, reads only its bucket, and rejects state writes, deletes, and lock creation")
	nextEnv := append(append([]string{}, qaEnv...), assignment("TF_VAR_payload", payload+"-changed"))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	process := exec.CommandContext(ctx, "tofu", "apply", "-input=true", "-no-color")
	process.Dir = qaRoot
	process.Env = nextEnv
	input, err := process.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	process.Stdout = io.Discard
	process.Stderr = io.Discard
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	locked := false
	for attempt := 0; attempt < 100; attempt++ {
		status, _, err := qa.request("GET", "/"+qaBucket+"/tack-qa.tfstate.tflock", nil)
		if err == nil && status == 200 {
			locked = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !locked {
		input.Write([]byte("no\n"))
		input.Close()
		process.Wait()
		t.Fatal("QA apply did not acquire state lock")
	}
	if output, err := tofu(qaRoot, qaEnv, "plan", "-lock-timeout=0s", "-input=false", "-no-color"); err == nil || !bytes.Contains(output, []byte("Error acquiring the state lock")) {
		t.Fatal("Concurrent QA plan did not reject the lock")
	}
	if output, err := tofu(prodRoot, prodEnv, "plan", "-input=false", "-no-color"); err != nil {
		t.Fatalf("Separate production bucket plan failed: %v: %s", err, output)
	}
	input.Write([]byte("no\n"))
	input.Close()
	process.Wait()
	requireStatus(t, qa, "GET", "/"+qaBucket+"/tack-qa.tfstate.tflock", nil, 404)
	if output, err := tofu(qaRoot, qaEnv, "plan", "-input=false", "-no-color"); err != nil {
		t.Fatalf("QA plan after normal release failed: %v: %s", err, output)
	}
	if !bytes.Equal(prodState, requireStatus(t, prod, "GET", "/"+prodBucket+"/tack-prod.tfstate", nil, 200)) {
		t.Fatal("QA operations changed production state")
	}
	t.Log("Concurrent lock denial, independent production apply/plan, normal release, and state isolation passed without temporary session tokens")
}
