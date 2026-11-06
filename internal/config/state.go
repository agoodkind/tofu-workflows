package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/agoodkind/tofu-workflows/internal/input"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"
)

var bucketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// ValidateStateOwnership compares physical S3 objects independently of state labels.
// Remote workspace existence requires authenticated backend initialization before execution.
func ValidateStateOwnership(set DeploymentSet, baseDir string) error {
	identities := make(map[string]string)
	for _, environment := range []Environment{set.Validation, set.Production} {
		identity, err := stateIdentity(environment, baseDir)
		if err != nil {
			slog.Error("State identity validation failed", "environment", environment.Name, "err", err)
			return fmt.Errorf("environment %q: %w", environment.Name, err)
		}
		if owner, exists := identities[identity]; exists {
			return fmt.Errorf("duplicate physical state identity in environments %q and %q", owner, environment.Name)
		}
		identities[identity] = environment.Name
	}
	return nil
}

func stateIdentity(environment Environment, baseDir string) (string, error) {
	root, err := input.Resolve(baseDir, environment.Configuration.Root)
	if err != nil {
		slog.Error("Configuration root resolution failed", "err", err)
		return "", fmt.Errorf("resolve configuration root: %w", err)
	}
	attributes, err := backendAttributes(root)
	if err != nil {
		return "", err
	}
	backendPath, err := input.RegularFile(baseDir, environment.BackendFile)
	if err != nil {
		return "", fmt.Errorf("resolve backend file: %w", err)
	}
	backend, err := parseHCL(backendPath)
	if err != nil {
		return "", err
	}
	overrides, diagnostics := backend.Body.JustAttributes()
	if diagnostics.HasErrors() {
		return "", errors.New("backend_file must contain HCL attributes")
	}
	maps.Copy(attributes, overrides)
	if err := validateConfigurationFiles(environment, baseDir, root); err != nil {
		return "", err
	}
	return s3Identity(attributes, environment.CLIWorkspace)
}

func validateConfigurationFiles(environment Environment, baseDir, root string) error {
	for _, name := range environment.VariableFiles {
		if _, err := input.RegularFile(baseDir, name); err != nil {
			slog.Error("Variable file validation failed", "err", err)
			return fmt.Errorf("variable file: %w", err)
		}
	}
	lockPath, err := input.RegularFile(root, ".terraform.lock.hcl")
	if err != nil {
		return fmt.Errorf("provider lock: %w", err)
	}
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return fmt.Errorf("read provider lock: %w", err)
	}
	if fmt.Sprintf("sha256:%x", sha256.Sum256(data)) != environment.Configuration.ProviderLockDigest {
		return errors.New("provider lock digest does not match configuration.provider_lock_digest")
	}
	return nil
}

func parseHCL(filename string) (*hcl.File, error) {
	parser := hclparse.NewParser()
	var file *hcl.File
	var diagnostics hcl.Diagnostics
	if strings.HasSuffix(filename, ".json") {
		file, diagnostics = parser.ParseJSONFile(filename)
	} else {
		file, diagnostics = parser.ParseHCLFile(filename)
	}
	if diagnostics.HasErrors() {
		return nil, fmt.Errorf("invalid HCL in %q", filename)
	}
	return file, nil
}

func backendAttributes(root string) (hcl.Attributes, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		slog.Error("Configuration directory read failed", "err", err)
		return nil, fmt.Errorf("read configuration root: %w", err)
	}
	var attributes hcl.Attributes
	files := make(map[string]bool)
	for _, entry := range entries {
		if !entry.IsDir() {
			files[entry.Name()] = true
		}
	}
	for _, entry := range entries {
		if !configurationFile(entry.Name(), files) {
			continue
		}
		filename, err := input.RegularFile(root, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("resolve configuration file: %w", err)
		}
		file, err := parseHCL(filename)
		if err != nil {
			return nil, err
		}
		blocks, _, diagnostics := file.Body.PartialContent(&hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "terraform"}}})
		if diagnostics.HasErrors() {
			return nil, errors.New("invalid terraform block")
		}
		for _, block := range blocks.Blocks {
			found, err := terraformBackend(block.Body)
			if err != nil {
				return nil, err
			}
			if found == nil {
				continue
			}
			if attributes != nil {
				return nil, errors.New("configuration must declare exactly one backend")
			}
			attributes = found
		}
	}
	if attributes == nil {
		return nil, errors.New("configuration must declare an s3 backend")
	}
	return attributes, nil
}

func configurationFile(name string, files map[string]bool) bool {
	if !files[name] || strings.HasPrefix(name, ".") {
		return false
	}
	for _, extension := range []string{".tf.json", ".tf"} {
		if base, matches := strings.CutSuffix(name, extension); matches {
			return !files[base+strings.ReplaceAll(extension, ".tf", ".tofu")]
		}
	}
	return strings.HasSuffix(name, ".tofu") || strings.HasSuffix(name, ".tofu.json")
}

func terraformBackend(body hcl.Body) (hcl.Attributes, error) {
	content, _, diagnostics := body.PartialContent(&hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "backend", LabelNames: []string{"type"}}}})
	if diagnostics.HasErrors() {
		return nil, errors.New("invalid backend declaration")
	}
	if len(content.Blocks) == 0 {
		return nil, nil
	}
	if len(content.Blocks) != 1 || content.Blocks[0].Labels[0] != "s3" {
		return nil, errors.New("only one s3 backend is supported")
	}
	attributes, diagnostics := content.Blocks[0].Body.JustAttributes()
	if diagnostics.HasErrors() {
		return nil, errors.New("invalid s3 backend attributes")
	}
	return attributes, nil
}

func attributeString(attributes hcl.Attributes, name string) (string, error) {
	attribute, exists := attributes[name]
	if !exists {
		return "", fmt.Errorf("s3 backend requires %s", name)
	}
	value, diagnostics := attribute.Expr.Value(nil)
	if diagnostics.HasErrors() || value.IsNull() || !value.IsKnown() || value.Type() != cty.String {
		return "", fmt.Errorf("s3 backend %s must be a literal string", name)
	}
	return value.AsString(), nil
}

func s3Endpoint(attributes hcl.Attributes) (string, error) {
	if endpoints, exists := attributes["endpoints"]; exists {
		if _, duplicate := attributes["endpoint"]; duplicate {
			return "", errors.New("s3 backend must not combine endpoint and endpoints")
		}
		value, diagnostics := endpoints.Expr.Value(nil)
		if diagnostics.HasErrors() || value.IsNull() || !value.IsWhollyKnown() || !value.Type().IsObjectType() || !value.Type().HasAttribute("s3") {
			return "", errors.New("s3 backend endpoints must declare a literal s3 endpoint")
		}
		endpoint := value.GetAttr("s3")
		if endpoint.IsNull() || endpoint.Type() != cty.String {
			return "", errors.New("s3 endpoint must be a literal string")
		}
		return canonicalEndpoint(endpoint.AsString())
	}
	endpoint, err := attributeString(attributes, "endpoint")
	if err != nil {
		return "", err
	}
	return canonicalEndpoint(endpoint)
}

func canonicalEndpoint(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", errors.New("s3 endpoint must be an absolute HTTP or HTTPS URL")
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", errors.New("s3 endpoint must be an absolute HTTP or HTTPS URL without credentials, query, or fragment")
	}
	parsed.Host = strings.ToLower(parsed.Host)
	if (parsed.Scheme == "https" && parsed.Port() == "443") || (parsed.Scheme == "http" && parsed.Port() == "80") {
		parsed.Host = strings.TrimSuffix(parsed.Host, ":"+parsed.Port())
	}
	parsed.Path = strings.TrimSuffix(path.Clean("/"+parsed.Path), "/")
	parsed.RawPath = ""
	return parsed.String(), nil
}

func s3Identity(attributes hcl.Attributes, workspace string) (string, error) {
	endpoint, err := s3Endpoint(attributes)
	if err != nil {
		return "", err
	}
	bucket, err := attributeString(attributes, "bucket")
	if err != nil {
		return "", err
	}
	if err := validateBucketName(bucket); err != nil {
		return "", err
	}
	key, err := attributeString(attributes, "key")
	if err != nil {
		return "", err
	}
	if key == "" || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return "", errors.New("s3 key must identify a nonempty object")
	}
	if !workspacePattern.MatchString(workspace) {
		return "", errors.New("cli_workspace must be an explicit workspace name")
	}
	if workspace != "default" {
		prefix := "env:"
		if _, exists := attributes["workspace_key_prefix"]; exists {
			prefix, err = attributeString(attributes, "workspace_key_prefix")
			if err != nil {
				return "", err
			}
		}
		key = path.Join(prefix, workspace, key)
	}
	// The effective key includes the workspace prefix. Default and named workspaces
	// can select the same object despite different workspace names.
	identity := strings.Join([]string{endpoint, bucket, key}, "\x00")
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(identity))), nil
}

func validateBucketName(bucket string) error {
	if !bucketNamePattern.MatchString(bucket) {
		return errors.New("s3 bucket must contain 3 to 63 lowercase letters, digits, periods, or hyphens and begin and end with a letter or digit")
	}
	if strings.Contains(bucket, "..") {
		return errors.New("s3 bucket must not contain adjacent periods")
	}
	if _, err := netip.ParseAddr(bucket); err == nil {
		return errors.New("s3 bucket must not be an IP address")
	}
	return nil
}
