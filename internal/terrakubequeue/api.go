package terrakubequeue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/agoodkind/tofu-workflows/internal/terrakubequeue/reconcile"
	"github.com/google/uuid"
)

const (
	httpPathSeparator   = "/"
	jobQueryPrefix      = jobAPIPath + "?"
	httpsScheme         = "https"
	workspaceType       = "workspace"
	jobCommentPrefix    = "deployment-main-"
	organizationAPIPath = "/organization/"
	jobAPIPath          = "/job"
	workspaceAPIPath    = "/workspace/"
	jobFilterParameter  = "filter[job]"
	jobFilterFormat     = "workspace.id==%s;comments==%s;templateReference==%s"
	acceptHeader        = "Accept"
	contentTypeHeader   = "Content-Type"
	authorizationHeader = "Authorization"
	bearerPrefix        = "Bearer "
	deploymentAudience  = "terrakube-deploy"
	mediaType           = "application/vnd.api+json"
	jobType             = "job"
)

type apiClient struct {
	BaseURL      string
	Organization string
	Workspace    string
	Template     string
	Token        func(context.Context, string) (string, error)
	Client       *http.Client
}

func isCanonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)

	return err == nil && parsed.String() == value
}

type identifier struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

type attributes struct {
	Comments                        string           `json:"comments"`
	Template                        string           `json:"templateReference"`
	Status                          reconcile.Status `json:"status,omitempty"`
	Source                          string           `json:"source,omitempty"`
	Branch                          string           `json:"branch,omitempty"`
	DefaultTemplate                 string           `json:"defaultTemplate,omitempty"`
	RestrictApplyToConfiguredBranch bool             `json:"restrictApplyToConfiguredBranch,omitempty"`
	Commit                          string           `json:"commitId,omitempty"`
}

type resource struct {
	identifier

	Attributes    attributes `json:"attributes"`
	Relationships struct {
		Workspace struct {
			Data identifier `json:"data"`
		} `json:"workspace"`
	} `json:"relationships"`
}

type envelope[T resource | []resource] struct {
	Data T `json:"data"`
}

func request[T resource | []resource](
	ctx context.Context,
	client apiClient,
	method string,
	suffix string,
	body io.Reader,
	output *envelope[T],
) error {
	endpoint := strings.TrimRight(
		client.BaseURL,
		httpPathSeparator,
	) + organizationAPIPath + client.Organization + suffix
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		slog.ErrorContext(ctx, "create Terrakube request", "error", err)

		return fmt.Errorf("prepare job API request: %w", err)
	}

	if err := client.authorize(ctx, req); err != nil {
		return err
	}

	req.Header.Set(acceptHeader, mediaType)
	if body != nil {
		req.Header.Set(contentTypeHeader, mediaType)
	}

	res, err := client.Client.Do(req)
	if err != nil {
		return fmt.Errorf("send job API request: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf(
			"%w: Terrakube %s job request returned %s",
			reconcile.ErrQueue,
			method,
			res.Status,
		)
	}

	if err := json.NewDecoder(res.Body).Decode(output); err != nil {
		return fmt.Errorf("decode job API response: %w", err)
	}

	return nil
}

func (client apiClient) Submit(ctx context.Context, sha string) (string, error) {
	data, err := json.Marshal(envelope[resource]{Data: client.job(sha)})
	if err != nil {
		slog.ErrorContext(ctx, "encode Terrakube job", "error", err)

		return "", fmt.Errorf("encode job: %w", err)
	}

	var response envelope[resource]
	if err := request(
		ctx,
		client,
		http.MethodPost,
		jobAPIPath,
		bytes.NewReader(data),
		&response,
	); err != nil {
		return "", err
	}

	if response.Data.Type != jobType {
		return "", errMissingJob
	}

	return response.Data.ID, nil
}

func (client apiClient) Lookup(ctx context.Context, sha string) ([]reconcile.Job, error) {
	wanted := client.job(sha)
	query := url.Values{}
	query.Set(
		jobFilterParameter,
		fmt.Sprintf(jobFilterFormat, client.Workspace, wanted.Attributes.Comments, client.Template),
	)
	var response envelope[[]resource]
	if err := request(
		ctx,
		client,
		http.MethodGet,
		jobQueryPrefix+query.Encode(),
		nil,
		&response,
	); err != nil {
		return nil, err
	}

	var jobs []reconcile.Job

	for _, record := range response.Data {
		if record.Type != jobType || record.ID == "" ||
			record.Attributes.Comments != wanted.Attributes.Comments ||
			record.Attributes.Template != client.Template ||
			record.Relationships.Workspace.Data.ID != client.Workspace {
			return nil, errUnexpectedJob
		}

		jobs = append(
			jobs,
			reconcile.Job{
				ID:     record.ID,
				Status: record.Attributes.Status,
				Commit: record.Attributes.Commit,
			},
		)
	}

	return jobs, nil
}

func (client apiClient) validate() error {
	u, err := url.Parse(client.BaseURL)
	if err != nil || u.Scheme != httpsScheme || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" ||
		u.Fragment != "" {
		return errAPIURL
	}

	if !isCanonicalUUID(client.Organization) || !isCanonicalUUID(client.Workspace) ||
		!isCanonicalUUID(client.Template) ||
		client.Token == nil {
		return errMissingAPISettings
	}

	return nil
}

func (client apiClient) job(sha string) resource {
	var record resource
	record.Type = jobType
	record.Attributes = attributes{Comments: jobCommentPrefix + sha, Template: client.Template}
	record.Relationships.Workspace.Data = identifier{Type: workspaceType, ID: client.Workspace}

	return record
}

func (client apiClient) verifyWorkspace(ctx context.Context, branch, expectedOrigin string) error {
	var response envelope[resource]
	if err := request(
		ctx,
		client,
		http.MethodGet,
		workspaceAPIPath+client.Workspace,
		nil,
		&response,
	); err != nil {
		return err
	}

	workspace := response.Data.Attributes
	if response.Data.Type != workspaceType || response.Data.ID != client.Workspace ||
		workspace.Branch != branch ||
		!workspace.RestrictApplyToConfiguredBranch ||
		workspace.DefaultTemplate != client.Template {
		return errWorkspacePolicy
	}

	if !matchesPromotionRepository(workspace.Source, expectedOrigin) {
		return errWorkspaceSource
	}

	return nil
}

func matchesPromotionRepository(actual, expected string) bool {
	return expected != "" && (actual == expected || actual == expected+gitRepositorySuffix)
}

var (
	errAPIURL             = errors.New("configure an HTTPS API base URL")
	errMissingAPISettings = errors.New(
		"configure organization, workspace and template UUIDs and GitHub OIDC",
	)
	errMissingJob      = errors.New("job API did not return a job")
	errUnexpectedJob   = errors.New("job API returned a job outside the requested submission")
	errWorkspacePolicy = errors.New("workspace branch or deployment template does not match")
	errWorkspaceSource = errors.New(
		"workspace must use the GitHub repository selected by the deployment job",
	)
)

func (client apiClient) authorize(ctx context.Context, req *http.Request) error {
	token, err := client.Token(ctx, deploymentAudience)
	if err != nil || token == "" {
		return errOIDC
	}

	req.Header.Set(authorizationHeader, bearerPrefix+token)

	return nil
}

var errOIDC = errors.New("GitHub did not issue a Terrakube deployment identity token")
