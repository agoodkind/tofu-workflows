// Package config validates protected deployment configuration.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/agoodkind/tofu-workflows/internal/input"
	strictjson "sigs.k8s.io/json"
)

// TofuVersion pins the supported OpenTofu runtime.
const TofuVersion = "1.13.2"

// Configuration selects an exact OpenTofu configuration and provider lock.
type Configuration struct {
	Repository         string `json:"repository"`
	Revision           string `json:"revision"`
	Root               string `json:"root"`
	TofuVersion        string `json:"tofu_version"`
	ProviderLockDigest string `json:"provider_lock_digest"`
}

// Environment defines one deployment stage and its protected execution
// settings.
type Environment struct {
	Name                       string            `json:"name"`
	GitHubPlanEnvironment      string            `json:"github_plan_environment"`
	GitHubApplyEnvironment     string            `json:"github_apply_environment"`
	Configuration              Configuration     `json:"configuration"`
	CLIWorkspace               string            `json:"cli_workspace"`
	BackendFile                string            `json:"backend_file"`
	StateID                    string            `json:"state_id"`
	Parallelism                int               `json:"parallelism"`
	JobTimeoutMinutes          int               `json:"job_timeout_minutes"`
	AccessTunnels              []AccessTunnel    `json:"access_tunnels"`
	HostLocks                  []HostLockTarget  `json:"host_locks"`
	VariableFiles              []string          `json:"variable_files"`
	BackendSecretEnvNames      []string          `json:"backend_secret_env_names"`
	SecretEnvNames             []string          `json:"secret_env_names"`
	SecretFiles                map[string]string `json:"secret_files"`
	HealthCheck                Check             `json:"health_check"`
	InspectCheck               Check             `json:"inspect_check"`
	CleanupCheck               *Check            `json:"cleanup_check"`
	RecoveryVariableNames      []string          `json:"recovery_variable_names"`
	RecoveryResourceAddresses  []string          `json:"recovery_resource_addresses"`
	ProtectedResourceAddresses []string          `json:"protected_resource_addresses"`
}

// Check selects a trusted container and limits its execution.
type Check struct {
	Image           string   `json:"image"`
	Arguments       []string `json:"arguments"`
	TimeoutSeconds  int      `json:"timeout_seconds"`
	SecretEnvNames  []string `json:"secret_env_names"`
	SecretFileNames []string `json:"secret_file_names"`
}

// AccessTunnel selects one fixed Access endpoint and its credential references.
type AccessTunnel struct {
	ID              string `json:"id"`
	Hostname        string `json:"hostname"`
	LocalPort       int    `json:"local_port"`
	ClientIDEnv     string `json:"client_id_env"`
	ClientSecretEnv string `json:"client_secret_env"`
}

// HostLockTarget identifies a host and its protected SSH connection settings.
type HostLockTarget struct {
	ID             string `json:"id"`
	Address        string `json:"address"`
	ConnectAddress string `json:"connect_address"`
	User           string `json:"user"`
	PrivateKeyFile string `json:"private_key_file"`
	KnownHostsFile string `json:"known_hosts_file"`
}

// VariableRule restricts one scalar release variable.
type VariableRule struct {
	Kind          string   `json:"kind"`
	AllowedValues []string `json:"allowed_values"`
}

// DeploymentSet defines versioned release policy for validation and production.
type DeploymentSet struct {
	Version              int                     `json:"version"`
	ID                   string                  `json:"id"`
	SourceBranch         string                  `json:"source_branch"`
	InitialCommit        string                  `json:"initial_commit"`
	ProducerWorkflowID   int64                   `json:"producer_workflow_id"`
	RecorderActorID      int64                   `json:"recorder_actor_id"`
	RequiredChecks       []CheckIdentity         `json:"required_checks"`
	DispatchWorkflow     string                  `json:"dispatch_workflow"`
	ArtifactRepositories map[string]string       `json:"artifact_repositories"`
	Variables            map[string]VariableRule `json:"variables"`
	Validation           Environment             `json:"validation"`
	Production           Environment             `json:"production"`
}

// CheckIdentity requires a named check from a specific GitHub App.
type CheckIdentity struct {
	AppID int64  `json:"app_id"`
	Name  string `json:"name"`
}

var (
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	variablePattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	workspacePattern  = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// Load reads a deployment set and rejects invalid or undeclared configuration.
func Load(path string) (DeploymentSet, error) {
	var set DeploymentSet
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Error("Deployment configuration read failed", "err", err)
		return set, fmt.Errorf("read deployment set: %w", err)
	}

	strictErrors, decodeErr := strictjson.UnmarshalStrict(data, &set)
	if err := errors.Join(append(strictErrors, decodeErr)...); err != nil {
		return set, fmt.Errorf("decode deployment set: %w", err)
	}

	if err := validate(set); err != nil {
		return set, fmt.Errorf("validate deployment set: %w", err)
	}

	return set, nil
}

func validate(set DeploymentSet) error {
	if set.Version != 1 {
		return errors.New("version must be 1")
	}

	if set.ID == "" {
		return errors.New("id is required")
	}

	if set.SourceBranch == "" {
		return errors.New("source_branch is required")
	}

	if !input.Commit(set.InitialCommit) {
		return errors.New("initial_commit must be an exact 40-character hexadecimal commit")
	}

	if set.ProducerWorkflowID <= 0 {
		return errors.New("producer_workflow_id must be positive")
	}

	if set.RecorderActorID <= 0 {
		return errors.New("recorder_actor_id must be positive")
	}

	if set.DispatchWorkflow == "" {
		return errors.New("dispatch_workflow is required")
	}

	if len(set.RequiredChecks) == 0 {
		return errors.New("required_checks is required")
	}

	for _, check := range set.RequiredChecks {
		if check.AppID <= 0 || check.Name == "" {
			return errors.New("required_checks requires an app_id and name")
		}
	}

	if err := validateRules(set); err != nil {
		return err
	}

	for _, environment := range []Environment{set.Validation, set.Production} {
		if err := validateEnvironment(environment, set); err != nil {
			slog.Error(
				"Environment configuration validation failed",
				"environment",
				environment.Name,
				"err",
				err,
			)

			return fmt.Errorf("environment %q: %w", environment.Name, err)
		}
	}

	if set.Validation.Name == set.Production.Name {
		return errors.New("environment names must differ")
	}

	return nil
}

func validateRules(set DeploymentSet) error {
	for name, repository := range set.ArtifactRepositories {
		if !variablePattern.MatchString(name) || !input.ImageRepository(repository) {
			return errors.New(
				"artifact_repositories requires valid names and image repositories without tags",
			)
		}
	}

	for name, rule := range set.Variables {
		if !variablePattern.MatchString(name) {
			return fmt.Errorf("variable %q has an invalid name", name)
		}

		if !slices.Contains([]string{"string", "number", "bool"}, rule.Kind) {
			return fmt.Errorf("variable %q has an unsupported kind", name)
		}

		if rule.Kind != "string" && len(rule.AllowedValues) != 0 {
			return fmt.Errorf("variable %q allows values only for string kinds", name)
		}
	}

	return nil
}

func validateEnvironment(environment Environment, set DeploymentSet) error {
	if err := validateReferences(environment); err != nil {
		return err
	}

	if environment.Parallelism <= 0 {
		return errors.New("parallelism must be positive")
	}

	if environment.JobTimeoutMinutes < 1 || environment.JobTimeoutMinutes > 180 {
		return errors.New("job_timeout_minutes must be between 1 and 180")
	}

	if !workspacePattern.MatchString(environment.CLIWorkspace) {
		return errors.New("cli_workspace must be an explicit workspace name")
	}

	if err := validateSecrets(environment); err != nil {
		return err
	}

	checks := []Check{environment.HealthCheck, environment.InspectCheck}
	if environment.CleanupCheck != nil {
		checks = append(checks, *environment.CleanupCheck)
	}

	for _, check := range checks {
		if err := validateCheck(check, environment); err != nil {
			return err
		}
	}

	for _, name := range environment.RecoveryVariableNames {
		if _, exists := set.Variables[name]; !exists {
			return fmt.Errorf("recovery variable %q is not declared", name)
		}
	}

	for _, address := range environment.RecoveryResourceAddresses {
		if slices.Contains(environment.ProtectedResourceAddresses, address) {
			return errors.New("recovery_resource_addresses overlaps protected_resource_addresses")
		}
	}

	return validateConnections(environment)
}

func validateReferences(environment Environment) error {
	for name, value := range map[string]string{
		"name":                     environment.Name,
		"github_plan_environment":  environment.GitHubPlanEnvironment,
		"github_apply_environment": environment.GitHubApplyEnvironment,
		"configuration.root":       environment.Configuration.Root,
		"backend_file":             environment.BackendFile,
		"state_id":                 environment.StateID,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}

	if !repositoryPattern.MatchString(environment.Configuration.Repository) {
		return errors.New("configuration.repository must identify an owner and repository")
	}

	if !input.Commit(environment.Configuration.Revision) {
		return errors.New("configuration.revision must be an exact 40-character hexadecimal commit")
	}

	if environment.Configuration.TofuVersion != TofuVersion {
		return fmt.Errorf("configuration.tofu_version must be %s", TofuVersion)
	}

	if !input.Digest(environment.Configuration.ProviderLockDigest) {
		return errors.New("configuration.provider_lock_digest must be a sha256 digest")
	}

	return nil
}

func validateSecrets(environment Environment) error {
	names := make(map[string]bool)

	for _, name := range append(
		slices.Clone(environment.BackendSecretEnvNames),
		environment.SecretEnvNames...,
	) {
		if !SecretEnvironmentName(name) {
			return fmt.Errorf("secret environment name %q is not allowed", name)
		}

		if names[name] {
			return fmt.Errorf("secret environment name %q is duplicated or overlaps bundles", name)
		}

		names[name] = true
	}

	for name, path := range environment.SecretFiles {
		if !names[name] || path == "" {
			return fmt.Errorf("secret file %q must reference a declared secret and pathname", name)
		}
	}

	return nil
}

// SecretEnvironmentName rejects reserved names and invalid variable syntax.
func SecretEnvironmentName(name string) bool {
	if !variablePattern.MatchString(name) {
		return false
	}

	if slices.Contains([]string{"PATH", "HOME", "TMPDIR", "TERRAFORM_PRE_RUN"}, name) {
		return false
	}

	for _, prefix := range []string{"GITHUB_", "ACTIONS_", "INPUT_", "TF_CLI_ARGS", "TF_LOG"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}

	return true
}

func validateCheck(check Check, environment Environment) error {
	if !input.Image(check.Image) {
		return errors.New("check.image must identify an image by repository and sha256 digest")
	}

	if check.TimeoutSeconds <= 0 || check.TimeoutSeconds > environment.JobTimeoutMinutes*60 {
		return errors.New("check.timeout_seconds must be positive and fit the job timeout")
	}

	for _, name := range check.SecretEnvNames {
		if !slices.Contains(environment.SecretEnvNames, name) {
			return fmt.Errorf("check secret %q is not declared in secret_env_names", name)
		}
	}

	for _, name := range check.SecretFileNames {
		if _, exists := environment.SecretFiles[name]; !exists {
			return fmt.Errorf("check secret file %q is not declared", name)
		}
	}

	return nil
}

func validateConnections(environment Environment) error {
	ports := make(map[int]bool)
	identities := make(map[string]bool)

	for _, tunnel := range environment.AccessTunnels {
		if tunnel.ID == "" || identities[tunnel.ID] {
			return errors.New("access tunnel IDs must be nonempty and unique")
		}

		identities[tunnel.ID] = true
		if tunnel.Hostname == "" || strings.ContainsAny(tunnel.Hostname, "/:@ \\?") {
			return errors.New("access tunnel hostname must identify a fixed host")
		}

		if tunnel.LocalPort < 1 || tunnel.LocalPort > 65535 || ports[tunnel.LocalPort] {
			return errors.New("access tunnel ports must be valid and unique")
		}

		ports[tunnel.LocalPort] = true
		for _, name := range []string{tunnel.ClientIDEnv, tunnel.ClientSecretEnv} {
			if !slices.Contains(environment.SecretEnvNames, name) {
				return errors.New(
					"access tunnel credentials must reference declared secret_env_names",
				)
			}
		}
	}

	for _, host := range environment.HostLocks {
		if host.ID == "" || host.Address == "" || host.User == "" || host.PrivateKeyFile == "" ||
			host.KnownHostsFile == "" {
			return errors.New(
				"host locks require id, address, user, private_key_file, and known_hosts_file",
			)
		}
	}

	return nil
}
