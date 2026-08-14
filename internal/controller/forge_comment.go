package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	commentMarker = "<!-- preview-operator-comment -->"

	// ProviderGitHub selects the GitHub REST API (api.github.com).
	ProviderGitHub = "github"
	// ProviderGitea selects the Gitea REST API. Forgejo is a Gitea fork and
	// serves the same /api/v1 surface, so self-hosted Forgejo uses this too.
	ProviderGitea = "gitea"

	// githubAPIBaseURL is the default API root for ProviderGitHub.
	githubAPIBaseURL = "https://api.github.com"
	// giteaAPIPath is the path prefix every Gitea/Forgejo API route sits under.
	giteaAPIPath = "/api/v1"

	// forgeRequestTimeout bounds a single call to the forge API so a hung
	// endpoint cannot stall a reconcile.
	forgeRequestTimeout = 30 * time.Second
)

// ForgeConfig identifies the forge hosting the pull request to comment on.
type ForgeConfig struct {
	// Provider is ProviderGitHub or ProviderGitea. Empty means ProviderGitHub.
	Provider string
	// APIBaseURL is the API root. Empty defaults to https://api.github.com for
	// GitHub; for Gitea/Forgejo it must be supplied (the caller derives it from
	// the Flux GitRepository URL when it is not configured explicitly).
	APIBaseURL string
	// Repo is the "owner/repo" slug the PR lives in.
	Repo string
}

// forgeComment represents an issue/PR comment. GitHub and Gitea/Forgejo return
// the same JSON shape for the fields the operator cares about.
type forgeComment struct {
	ID   int    `json:"id"`
	Body string `json:"body"`
}

// commentClient posts the preview-URL comment on a pull request.
type commentClient interface {
	// FindComment returns this operator's existing comment on the PR, or nil
	// when it has not commented yet.
	FindComment(ctx context.Context, repo, prNumber string) (*forgeComment, error)
	CreateComment(ctx context.Context, repo, prNumber, body string) error
	UpdateComment(ctx context.Context, repo string, commentID int, body string) error
}

// newCommentClient builds the client for the configured provider.
func newCommentClient(cfg ForgeConfig, token string) (commentClient, error) {
	switch cfg.Provider {
	case "", ProviderGitHub:
		return newGitHubClient(cfg.APIBaseURL, token), nil
	case ProviderGitea:
		if cfg.APIBaseURL == "" {
			return nil, fmt.Errorf("git provider %q requires an API base URL", ProviderGitea)
		}
		return newGiteaClient(cfg.APIBaseURL, token), nil
	default:
		return nil, fmt.Errorf("unsupported git provider %q (want %q or %q)", cfg.Provider, ProviderGitHub, ProviderGitea)
	}
}

// PostPRComment posts or updates the preview-URL comment on the PR.
func PostPRComment(ctx context.Context, k8sClient client.Client, namespace string, forge ForgeConfig, prNumber, appName, previewURL string, log logr.Logger) error {
	token, err := getForgeToken(ctx, k8sClient, namespace)
	if err != nil {
		return fmt.Errorf("getting forge token: %w", err)
	}

	forgeClient, err := newCommentClient(forge, token)
	if err != nil {
		return err
	}

	body := buildCommentBody(appName, prNumber, previewURL)

	existing, err := forgeClient.FindComment(ctx, forge.Repo, prNumber)
	if err != nil {
		return fmt.Errorf("finding existing comment: %w", err)
	}

	if existing != nil {
		log.Info("Updating existing PR comment", "pr", prNumber, "commentID", existing.ID)
		return forgeClient.UpdateComment(ctx, forge.Repo, existing.ID, body)
	}

	log.Info("Creating new PR comment", "pr", prNumber)
	return forgeClient.CreateComment(ctx, forge.Repo, prNumber, body)
}

// getForgeToken reads the forge API token from the github-token secret in the
// given namespace. The secret keeps its historical name; Gitea/Forgejo tokens
// go in the same place, under either the "password" or "token" key.
func getForgeToken(ctx context.Context, k8sClient client.Client, namespace string) (string, error) {
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: namespace, Name: "github-token"}
	if err := k8sClient.Get(ctx, key, &secret); err != nil {
		return "", fmt.Errorf("reading github-token secret: %w", err)
	}

	for _, dataKey := range []string{"password", "token"} {
		if token := strings.TrimSpace(string(secret.Data[dataKey])); token != "" {
			return token, nil
		}
	}

	return "", fmt.Errorf("github-token secret missing 'password' or 'token' key")
}

// buildCommentBody constructs the markdown comment body with a preview URL.
func buildCommentBody(appName, prNumber, previewURL string) string {
	return fmt.Sprintf(`%s
## :rocket: Preview Environment Ready

**App:** `+"`%s`"+`
**PR:** #%s

:link: **Preview URL:** [%s](%s)

---
*Posted by homelab-preview-operator*`,
		commentMarker, appName, prNumber, previewURL, previewURL)
}

// apiBaseURLFromCloneURL derives a forge's web origin from a git clone URL, so
// a self-hosted Gitea/Forgejo needs no explicit API base URL: the Flux
// GitRepository already points at the host. Both https:// and SSH (including
// the scp-like git@host:owner/repo.git form) clone URLs are accepted; the
// result is always an https:// origin with no path.
func apiBaseURLFromCloneURL(cloneURL string) (string, error) {
	trimmed := strings.TrimSpace(cloneURL)
	if trimmed == "" {
		return "", fmt.Errorf("empty git clone URL")
	}

	// scp-like syntax (git@host:owner/repo.git) is not a parseable URL.
	if !strings.Contains(trimmed, "://") {
		host, _, found := strings.Cut(trimmed, ":")
		if !found {
			return "", fmt.Errorf("cannot derive API base URL from %q", cloneURL)
		}
		if _, after, ok := strings.Cut(host, "@"); ok {
			host = after
		}
		if host == "" {
			return "", fmt.Errorf("cannot derive API base URL from %q", cloneURL)
		}
		return "https://" + host, nil
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parsing git clone URL %q: %w", cloneURL, err)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("git clone URL %q has no host", cloneURL)
	}

	// url.URL.Host is already free of any embedded credentials.
	if parsed.Scheme == "http" || parsed.Scheme == "https" {
		return parsed.Scheme + "://" + parsed.Host, nil
	}

	// ssh:// and git:// clone URLs: the API is served over https on the
	// standard port, so the SSH port is dropped.
	return "https://" + parsed.Hostname(), nil
}

// restCommentClient implements commentClient against the issue-comment REST
// API that GitHub and Gitea/Forgejo both expose — same routes, same status
// codes. Only the API root, the Accept header and the name used in error
// messages differ between the two.
type restCommentClient struct {
	baseURL  string
	token    string
	accept   string
	provider string
	client   *http.Client
}

// FindComment lists the PR's comments and returns the one carrying the
// operator's marker.
func (c *restCommentClient) FindComment(ctx context.Context, repo, prNumber string) (*forgeComment, error) {
	req, err := c.newRequest(ctx, http.MethodGet, c.commentsURL(repo, prNumber), "")
	if err != nil {
		return nil, err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing comments: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := c.checkStatus(resp, http.StatusOK); err != nil {
		return nil, err
	}

	var comments []forgeComment
	if err := json.NewDecoder(resp.Body).Decode(&comments); err != nil {
		return nil, fmt.Errorf("decoding comments: %w", err)
	}

	for i := range comments {
		if strings.Contains(comments[i].Body, commentMarker) {
			return &comments[i], nil
		}
	}
	return nil, nil
}

// CreateComment posts a new comment on the PR.
func (c *restCommentClient) CreateComment(ctx context.Context, repo, prNumber, body string) error {
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return err
	}

	req, err := c.newRequest(ctx, http.MethodPost, c.commentsURL(repo, prNumber), string(payload))
	if err != nil {
		return err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("creating comment: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	return c.checkStatus(resp, http.StatusCreated)
}

// UpdateComment rewrites an existing comment in place.
func (c *restCommentClient) UpdateComment(ctx context.Context, repo string, commentID int, body string) error {
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return err
	}

	req, err := c.newRequest(ctx, http.MethodPatch, c.commentURL(repo, commentID), string(payload))
	if err != nil {
		return err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("updating comment: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	return c.checkStatus(resp, http.StatusOK)
}

// commentsURL is the collection route: list and create.
func (c *restCommentClient) commentsURL(repo, prNumber string) string {
	return fmt.Sprintf("%s/repos/%s/issues/%s/comments", c.baseURL, repo, prNumber)
}

// commentURL is the single-comment route: update.
func (c *restCommentClient) commentURL(repo string, commentID int) string {
	return fmt.Sprintf("%s/repos/%s/issues/comments/%d", c.baseURL, repo, commentID)
}

func (c *restCommentClient) newRequest(ctx context.Context, method, url, body string) (*http.Request, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", c.accept)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *restCommentClient) checkStatus(resp *http.Response, want int) error {
	if resp.StatusCode == want {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("%s API returned %d: %s", c.provider, resp.StatusCode, string(body))
}
