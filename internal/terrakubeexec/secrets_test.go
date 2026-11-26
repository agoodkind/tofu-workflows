package terrakubeexec

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

const (
	referenceCommit = "1234567890123456789012345678901234567890"
	qaTokenName     = "OP_SERVICE_ACCOUNT_TOKEN_TACK_QA"
	fakeToken       = "synthetic-resolver-token"
	fakeValue       = "synthetic-field-value"
	referencesQA    = `{
		"version":1,
		"service":"tack",
		"deployment":"tack-qa",
		"vault":"tack-qa",
		"service_account_env":"OP_SERVICE_ACCOUNT_TOKEN_TACK_QA",
		"references":{
			"AWS_ACCESS_KEY_ID":"op://tack-qa/state/access-key-id",
			"AWS_SECRET_ACCESS_KEY":"op://tack-qa/state/secret-access-key",
			"CONFIGS_DEPLOY_PROVIDER_TOKEN":"op://tack-qa/providers/read-token",
			"CONFIGS_DEPLOY_SSH_PRIVATE_KEY":"op://tack-qa/deployment-ssh/private-key",
			"CONFIGS_DEPLOY_SSH_KNOWN_HOSTS":"op://tack-qa/deployment-ssh/known-hosts",
			"TF_VAR_vault_tack_qa_app_password":"op://tack-qa/opentofu/app-password",
			"TF_VAR_vault_tofu_state_passphrase":"op://tack-qa/state/encryption-passphrase"
		}
	}`
)

func TestDeployResolvesOnlyItsVerifiedWorkspaceReferences(t *testing.T) {
	workspace := []string{
		qaTokenName + "=" + fakeToken,
		"OP_CONNECT_TOKEN=synthetic-unrelated-token",
		"TF_VAR_unrelated=synthetic-unrelated-value",
	}
	base := []string{"HOME=/private/home", "PATH=/usr/local/bin:/usr/bin:/bin"}
	var reads []string
	resolver := func(directory string, environment []string) commandRunner {
		want := append(slices.Clone(base), "OP_CACHE=false", opToken+"="+fakeToken)
		if directory != "/private" || !slices.Equal(environment, want) {
			t.Fatal("the resolver did not receive exactly its isolated environment and account")
		}

		return func(_ context.Context, task command) error {
			if task.name != opCLI || len(task.args) != 3 || task.args[0] != "read" ||
				task.args[1] != "--no-newline" || !strings.HasPrefix(task.args[2], "op://tack-qa/") {
				t.Fatalf("unexpected 1Password command: %v", task.args)
			}

			_, field, _ := strings.Cut(strings.TrimPrefix(task.args[2], "op://tack-qa/"), "/")
			reads = append(reads, task.args[2])
			_, err := io.WriteString(task.output, fakeValue+"-"+field)

			return err
		}
	}
	secrets, err := resolveDeploymentSecrets(
		t.Context(), fakeReferences(t, referencesQA), referenceCommit,
		deploymentIdentity{service: "tack", deployment: "tack-qa"},
		workspace, resolver, "/private", base,
	)
	if err != nil || len(reads) != 7 {
		t.Fatalf("resolved %d fields, error = %v; want seven fields", len(reads), err)
	}

	assertDeploymentEnvironments(t, base, secrets)
}

func assertDeploymentEnvironments(t *testing.T, base []string, secrets deploymentSecrets) {
	t.Helper()
	tofu, install := secrets.commandEnvironments(base, "/private/agent.sock")
	common := append(slices.Clone(base), "SSH_AUTH_SOCK=/private/agent.sock", "TF_IN_AUTOMATION=1")
	expectedTofu := append(
		slices.Clone(common),
		"AWS_ACCESS_KEY_ID="+fakeValue+"-access-key-id",
		strings.Join([]string{"AWS_SECRET_ACCESS_KEY", fakeValue + "-secret-access-key"}, "="),
		strings.Join([]string{"TF_VAR_vault_tack_qa_app_password", fakeValue + "-app-password"}, "="),
		strings.Join([]string{"TF_VAR_vault_tofu_state_passphrase", fakeValue + "-encryption-passphrase"}, "="),
		secretEnvironment+"=AWS_ACCESS_KEY_ID,AWS_SECRET_ACCESS_KEY,TF_VAR_vault_tack_qa_app_password,TF_VAR_vault_tofu_state_passphrase",
	)
	if !slices.Equal(tofu, expectedTofu) {
		t.Fatal("configsctl OpenTofu environment omitted a declared input or included unrelated credentials")
	}

	if !slices.Equal(install, append(common, "GH_TOKEN="+fakeValue+"-read-token")) {
		t.Fatal("configsctl provider installation received unexpected credentials")
	}

	if secrets.sshKey != fakeValue+"-private-key" || secrets.sshKnownHosts != fakeValue+"-known-hosts" {
		t.Fatal("the resolver did not return the SSH inputs separately")
	}
}

func TestDeployRejectsInvalidWorkspaceSecretsBeforeStartingResolver(t *testing.T) {
	valid := []string{qaTokenName + "=" + fakeToken}
	for _, test := range []struct {
		name        string
		document    string
		environment []string
		wantError   error
	}{
		{"missing token", referencesQA, nil, errResolverScope},
		{"wrong workspace token", referencesQA, []string{"OP_SERVICE_ACCOUNT_TOKEN_TACK_PROD=" + fakeToken}, errResolverScope},
		{"generic token", referencesQA, []string{opToken + "=" + fakeToken}, errResolverScope},
		{"two workspace tokens", referencesQA, append(slices.Clone(valid), "OP_SERVICE_ACCOUNT_TOKEN_TACK_PROD="+fakeToken), errResolverScope},
		{"other deployment", strings.ReplaceAll(referencesQA, "tack-qa", "tack-prod"), valid, errReference},
		{"other vault", strings.Replace(referencesQA, "op://tack-qa/", "op://tack-prod/", 1), valid, errReference},
		{"resolver token reference", strings.Replace(referencesQA, "TF_VAR_vault_tack_qa_app_password", opToken, 1), valid, errReference},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolver := func(string, []string) commandRunner {
				t.Fatal("invalid workspace input started a resolver")
				return nil
			}
			_, err := resolveDeploymentSecrets(
				t.Context(), fakeReferences(t, test.document), referenceCommit,
				deploymentIdentity{service: "tack", deployment: "tack-qa"},
				test.environment, resolver, "/private", nil,
			)
			if !errors.Is(err, test.wantError) {
				t.Fatalf("resolve deployment secrets: %v; want %v", err, test.wantError)
			}
		})
	}
}

func TestDeployDoesNotExposeTheResolverResponseWhenReadFails(t *testing.T) {
	resolver := func(string, []string) commandRunner {
		return func(_ context.Context, task command) error {
			_, _ = io.WriteString(task.output, fakeValue)
			return errors.New(fakeToken)
		}
	}
	_, err := resolveDeploymentSecrets(
		t.Context(), fakeReferences(t, referencesQA), referenceCommit,
		deploymentIdentity{service: "tack", deployment: "tack-qa"},
		[]string{qaTokenName + "=" + fakeToken}, resolver, "/private", nil,
	)
	if !errors.Is(err, errSecretRead) || strings.Contains(err.Error(), fakeToken) ||
		strings.Contains(err.Error(), fakeValue) || !strings.Contains(err.Error(), "AWS_ACCESS_KEY_ID") {
		t.Fatal("resolver failure did not identify only the affected variable")
	}
}

func fakeReferences(t *testing.T, document string) commandRunner {
	t.Helper()

	return func(_ context.Context, task command) error {
		object := referenceCommit + ":.github/terrakube/secrets/tack/tack-qa.json"
		if task.name != "/usr/bin/git" || !slices.Equal(task.args, []string{"show", object}) {
			t.Fatalf("did not read the selected references from the verified revision: %v", task.args)
		}

		_, err := io.WriteString(task.output, document)

		return err
	}
}
