package terrakubeexec

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	strictjson "sigs.k8s.io/json"
)

const (
	opCLI                   = "/usr/local/bin/op"
	opToken                 = "OP_SERVICE_ACCOUNT_TOKEN"
	secretDeclarations      = ".github/terrakube/secrets/"
	secretEnvironment       = "CONFIGSCTL_TOFU_SECRET_ENVIRONMENT"
	providerToken           = "CONFIGS_DEPLOY_PROVIDER_TOKEN"
	sshKey                  = "CONFIGS_DEPLOY_SSH_PRIVATE_KEY"
	sshKnownHosts           = "CONFIGS_DEPLOY_SSH_KNOWN_HOSTS"
	statePassphraseVariable = "TF_VAR_vault_tofu_state_passphrase"
	maxReferenceBytes       = 65536
	minSecretBytes          = 16
)

var (
	tofuVariable     = regexp.MustCompile(`^TF_VAR_[A-Za-z_][A-Za-z0-9_]*$`)
	referencePart    = regexp.MustCompile(`^[A-Za-z0-9_ .-]+$`)
	requiredSecrets  = []string{providerToken, sshKey, sshKnownHosts, statePassphraseVariable, "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}
	errReference     = errors.New("invalid Configs deployment secret reference manifest")
	errResolverScope = errors.New("inject exactly one protected workspace 1Password service account token")
	errSecretRead    = errors.New("1Password CLI could not resolve the required field")
	errSecretValue   = errors.New("1Password field is empty, larger than 64 KiB or invalid as an environment value")
)

type secretManifest struct {
	Version           int               `json:"version"`
	Service           string            `json:"service"`
	Deployment        string            `json:"deployment"`
	Vault             string            `json:"vault"`
	ServiceAccountEnv string            `json:"service_account_env"`
	References        map[string]string `json:"references"`
}

type deploymentSecrets struct {
	tofu          []string
	providerToken string
	sshKey        string
	sshKnownHosts string
}

type runnerFactory func(string, []string) commandRunner

func resolveDeploymentSecrets(
	ctx context.Context,
	git commandRunner,
	commit string,
	identity deploymentIdentity,
	environment []string,
	resolver runnerFactory,
	directory string,
	isolated []string,
) (deploymentSecrets, error) {
	path := secretDeclarations + identity.service + "/" + identity.deployment + ".json"
	manifestText, err := gitOutput(ctx, git, "show", commit+":"+path)
	if err != nil {
		return deploymentSecrets{}, fmt.Errorf("read Configs secret references for %s: %w", identity.deployment, err)
	}

	manifest, err := parseSecretManifest(manifestText, identity)
	if err != nil {
		return deploymentSecrets{}, err
	}

	token, err := workspaceServiceAccount(environment, manifest.ServiceAccountEnv)
	if err != nil {
		return deploymentSecrets{}, err
	}

	op := resolver(directory, append(slices.Clone(isolated), "OP_CACHE=false", opToken+"="+token))

	return readDeploymentSecrets(ctx, op, manifest.References)
}

func (secrets deploymentSecrets) commandEnvironments(base []string, socket string) (tofu, install []string) {
	deployment := append(slices.Clone(base), "SSH_AUTH_SOCK="+socket, "TF_IN_AUTOMATION=1")

	return append(slices.Clone(deployment), secrets.tofu...),
		append(deployment, "GH_TOKEN="+secrets.providerToken)
}

func parseSecretManifest(raw string, identity deploymentIdentity) (secretManifest, error) {
	var manifest secretManifest
	if len(raw) > maxReferenceBytes {
		return manifest, fmt.Errorf("%w: manifest exceeds 64 KiB", errReference)
	}

	strictErrors, decodeErr := strictjson.UnmarshalStrict([]byte(raw), &manifest)
	if decodeErr != nil || len(strictErrors) > 0 {
		return manifest, fmt.Errorf("%w: use the version 1 manifest schema", errReference)
	}

	expectedToken := opToken + "_" + strings.ToUpper(strings.ReplaceAll(identity.deployment, "-", "_"))
	if manifest.Version != 1 || manifest.Service != identity.service ||
		manifest.Deployment != identity.deployment || manifest.Vault != identity.deployment ||
		manifest.ServiceAccountEnv != expectedToken {
		return manifest, fmt.Errorf("%w: service, deployment, vault and token must match the verified enrollment", errReference)
	}

	for _, name := range requiredSecrets {
		if manifest.References[name] == "" {
			return manifest, fmt.Errorf("%w: declare %s for %s", errReference, name, identity.deployment)
		}
	}

	for name, reference := range manifest.References {
		if !allowedSecretVariable(name) {
			return manifest, fmt.Errorf("%w: use only supported environment variables", errReference)
		}

		if !validSecretReference(reference, manifest.Vault) {
			return manifest, fmt.Errorf("%w: %s must reference a field in the deployment vault", errReference, name)
		}
	}

	return manifest, nil
}

func allowedSecretVariable(name string) bool {
	return tofuVariable.MatchString(name) || slices.Contains(requiredSecrets, name) || name == "AWS_SESSION_TOKEN"
}

func validSecretReference(reference, vault string) bool {
	field, ok := strings.CutPrefix(reference, "op://"+vault+"/")
	if !ok {
		return false
	}

	parts := strings.Split(field, "/")
	if len(parts) != 2 && len(parts) != 3 {
		return false
	}

	for _, part := range parts {
		if part == "." || part == ".." || strings.TrimSpace(part) != part || !referencePart.MatchString(part) {
			return false
		}
	}

	return true
}

func workspaceServiceAccount(environment []string, expected string) (string, error) {
	var token string
	for _, entry := range environment {
		name, value, _ := strings.Cut(entry, "=")
		if name != opToken && !strings.HasPrefix(name, opToken+"_") {
			continue
		}

		if name != expected || token != "" || strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("%w in %s", errResolverScope, expected)
		}

		token = value
	}

	if token == "" {
		return "", fmt.Errorf("%w in %s", errResolverScope, expected)
	}

	return token, nil
}

func readDeploymentSecrets(
	ctx context.Context,
	op commandRunner,
	references map[string]string,
) (deploymentSecrets, error) {
	var secrets deploymentSecrets
	var tofuNames []string

	for _, name := range slices.Sorted(maps.Keys(references)) {
		var output secretOutput
		if err := op(ctx, command{
			name:   opCLI,
			args:   []string{"read", "--no-newline", references[name]},
			output: &output,
		}); err != nil {
			return deploymentSecrets{}, fmt.Errorf("%w: %s", errSecretRead, name)
		}

		value := output.String()
		if strings.TrimSpace(value) == "" || strings.ContainsRune(value, 0) {
			return deploymentSecrets{}, fmt.Errorf("%w: %s", errSecretValue, name)
		}

		switch name {
		case providerToken:
			secrets.providerToken = value
		case sshKey:
			secrets.sshKey = value
		case sshKnownHosts:
			secrets.sshKnownHosts = value
		default:
			if len(value) < minSecretBytes {
				return deploymentSecrets{}, fmt.Errorf("%s must contain at least %d bytes for configsctl redaction", name, minSecretBytes)
			}

			secrets.tofu = append(secrets.tofu, name+"="+value)
			tofuNames = append(tofuNames, name)
		}
	}

	secrets.tofu = append(secrets.tofu, secretEnvironment+"="+strings.Join(tofuNames, ","))

	return secrets, nil
}

type secretOutput struct {
	strings.Builder
}

func (output *secretOutput) Write(data []byte) (int, error) {
	if output.Len()+len(data) > maxReferenceBytes {
		return 0, errSecretValue
	}

	return output.Builder.Write(data)
}
