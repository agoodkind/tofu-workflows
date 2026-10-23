package r2proof

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

const bucket = "deployment-readiness-proof-20261023"
const endpoint = "https://ee7d7ca7d611ef8c2a07885e8362de0c.r2.cloudflarestorage.com"

type storage struct{ endpoint, access, secret, token string }

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
	req.Header.Set("x-amz-security-token", s.token)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	headers := "host:" + req.URL.Host + "\nx-amz-content-sha256:" + payloadHash + "\nx-amz-date:" + stamp + "\nx-amz-security-token:" + s.token + "\n"
	signed := "host;x-amz-content-sha256;x-amz-date;x-amz-security-token"
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
	return cmd.CombinedOutput()
}

func root(t *testing.T, base, name, endpoint string) string {
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
	inputs := struct{ Key, Endpoint string }{"proof/" + name + ".tfstate", endpoint}
	if err := tmpl.Execute(&backend, inputs); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "backend.hcl"), backend.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestEncryptedR2StateAndLocks(t *testing.T) {
	access, secret, token := os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), os.Getenv("AWS_SESSION_TOKEN")
	if access == "" || secret == "" || token == "" {
		t.Fatal("Scoped R2 credentials are required")
	}
	key := randomText(t)
	s := storage{endpoint: endpoint, access: access, secret: secret, token: token}
	t.Cleanup(func() {
		for _, name := range []string{"tack-qa.tfstate", "tack-prod.tfstate", "tack-qa.tfstate.tflock", "tack-prod.tfstate.tflock"} {
			status, _, err := s.request("DELETE", "/"+bucket+"/proof/"+name, nil)
			if err != nil || status != 204 {
				t.Errorf("Synthetic object cleanup failed: key=%s status=%d", name, status)
			}
		}
	})
	if status, _, err := s.request("PUT", "/"+bucket+"/denied/synthetic", []byte("denied")); err != nil || status != 403 {
		t.Fatalf("Out-of-scope synthetic write expected403: status=%d err=%v", status, err)
	}
	if status, _, err := s.request("GET", "/"+bucket+"/denied/synthetic", nil); err != nil || status != 403 {
		t.Fatalf("Out-of-scope synthetic read expected403: status=%d err=%v", status, err)
	}
	if status, _, err := s.request("GET", "/"+bucket+"?list-type=2&prefix=denied%2F", nil); err != nil || status != 403 {
		t.Fatalf("Out-of-scope list expected403: status=%d err=%v", status, err)
	}
	if status, _, err := s.request("GET", "/"+bucket+"?list-type=2&prefix=proof%2F", nil); err != nil || status != 200 {
		t.Fatalf("Scoped list expected200: status=%d err=%v", status, err)
	}
	t.Log("Scoped listing succeeded; out-of-scope synthetic reads, writes, and listing returned403")
	base := t.TempDir()
	qa := root(t, base, "tack-qa", s.endpoint)
	prod := root(t, base, "tack-prod", s.endpoint)
	payload := "synthetic-state-payload-" + randomText(t)
	cleanEnvironment := []string{}
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "AWS_") || strings.HasPrefix(entry, "TF_") {
			continue
		}
		cleanEnvironment = append(cleanEnvironment, entry)
	}
	environment := append(cleanEnvironment, "AWS_ACCESS_KEY_ID="+access, assignment("AWS_SECRET_ACCESS_KEY", secret), "AWS_SESSION_TOKEN="+token, "AWS_EC2_METADATA_DISABLED=true", "TF_VAR_vault_tofu_state_passphrase="+key, "TF_VAR_payload="+payload)
	for _, dir := range []string{qa, prod} {
		if output, err := tofu(dir, environment, "init", "-backend-config=backend.hcl", "-input=false", "-no-color"); err != nil {
			t.Fatalf("init failed: %v: %s", err, output)
		}
		if output, err := tofu(dir, environment, "plan", "-out=release.plan", "-input=false", "-no-color"); err != nil {
			t.Fatalf("plan failed: %v: %s", err, output)
		}
		plan, err := os.ReadFile(filepath.Join(dir, "release.plan"))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(plan, []byte(payload)) || !bytes.Contains(plan, []byte("encrypted_data")) {
			t.Fatal("saved plan did not satisfy encryption assertion")
		}
		if output, err := tofu(dir, environment, "apply", "-input=false", "-no-color", "release.plan"); err != nil {
			t.Fatalf("apply failed: %v: %s", err, output)
		}
	}
	_, qaState, err := s.request("GET", "/"+bucket+"/proof/tack-qa.tfstate", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, prodState, err := s.request("GET", "/"+bucket+"/proof/tack-prod.tfstate", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range [][]byte{qaState, prodState} {
		if bytes.Contains(state, []byte(payload)) || !bytes.Contains(state, []byte("encrypted_data")) {
			t.Fatal("stored state did not satisfy encryption assertion")
		}
	}
	if output, err := tofu(qa, environment, "output", "-raw", "proof"); err != nil || string(output) != payload {
		t.Fatal("correct-key output did not recover synthetic payload")
	}
	t.Log("Both R2 keys contain encrypted state; saved plans omit the synthetic payload; correct key decrypts it")

	missingKey := append(append([]string{}, cleanEnvironment...), "AWS_ACCESS_KEY_ID="+access, assignment("AWS_SECRET_ACCESS_KEY", secret), "AWS_SESSION_TOKEN="+token, "AWS_EC2_METADATA_DISABLED=true", "TF_VAR_payload="+payload)
	if output, err := tofu(qa, missingKey, "plan", "-input=false", "-no-color"); err == nil || !bytes.Contains(output, []byte("vault_tofu_state_passphrase")) {
		t.Fatal("missing passphrase did not fail")
	}
	wrongKey := append(append([]string{}, missingKey...), assignment("TF_VAR_vault_tofu_state_passphrase", randomText(t)))
	if output, err := tofu(qa, wrongKey, "plan", "-input=false", "-no-color"); err == nil || !bytes.Contains(bytes.ToLower(output), []byte("decrypt")) {
		t.Fatalf("wrong passphrase did not produce a decryption failure: %s", output)
	}
	if output, err := tofu(qa, wrongKey, "show", "-json", "release.plan"); err == nil || !bytes.Contains(bytes.ToLower(output), []byte("decrypt")) {
		t.Fatal("wrong passphrase did not prevent saved-plan decryption")
	}
	t.Log("Missing and incorrect state passphrases fail; incorrect passphrase also prevents saved-plan decryption")

	nextEnvironment := append(append([]string{}, environment...), "TF_VAR_payload="+payload+"-changed")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	holder := exec.CommandContext(ctx, "tofu", "apply", "-input=true", "-no-color")
	holder.Dir = qa
	holder.Env = nextEnvironment
	input, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var heldOutput bytes.Buffer
	holder.Stdout = &heldOutput
	holder.Stderr = &heldOutput
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	locked := false
	for attempt := 0; attempt < 100; attempt++ {
		status, _, err := s.request("GET", "/"+bucket+"/proof/tack-qa.tfstate.tflock", nil)
		if err == nil && status == 200 {
			locked = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !locked {
		input.Write([]byte("no\n"))
		input.Close()
		holder.Wait()
		t.Fatalf("first apply did not acquire lock: %s", heldOutput.String())
	}
	output, err := tofu(qa, environment, "plan", "-lock-timeout=0s", "-input=false", "-no-color")
	if err == nil || !bytes.Contains(output, []byte("Error acquiring the state lock")) {
		t.Fatal("concurrent plan did not reject the held QA lock")
	}
	if output, err := tofu(prod, environment, "plan", "-lock-timeout=0s", "-input=false", "-no-color"); err != nil {
		t.Fatalf("production key blocked by QA lock: %v: %s", err, output)
	}
	input.Write([]byte("no\n"))
	input.Close()
	holder.Wait()
	if status, _, err := s.request("GET", "/"+bucket+"/proof/tack-qa.tfstate.tflock", nil); err != nil || status != 404 {
		t.Fatalf("canceled apply did not release lock: status=%d err=%v", status, err)
	}
	if output, err := tofu(qa, environment, "plan", "-input=false", "-no-color"); err != nil {
		t.Fatalf("QA could not plan after normal lock release: %v: %s", err, output)
	}
	_, prodAfter, err := s.request("GET", "/"+bucket+"/proof/tack-prod.tfstate", nil)
	if err != nil || !bytes.Equal(prodState, prodAfter) {
		t.Fatal("QA operations changed production state")
	}
	t.Log("Concurrent QA plan rejected; production key remained usable; canceled apply released lock without force-unlock; production state stayed identical")
	fmt.Fprintln(os.Stdout, "R2 encryption, saved-plan encryption, key rejection, independent backend keys, and concurrent locking passed")
}
