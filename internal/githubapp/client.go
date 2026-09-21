package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the GitHub.com REST API root.
const DefaultBaseURL = "https://api.github.com"

const (
	acceptHeader     = "application/vnd.github+json"
	apiVersionHeader = "2022-11-28"

	// tokenRenewMargin is how long before its stated expiry a cached
	// installation token is considered spent.
	tokenRenewMargin = 5 * time.Minute

	// errBodyLimit is how much of a failing response body is quoted back in
	// the returned error.
	errBodyLimit = 512
)

// CheckRun is the subset of the GitHub Check Runs API the operator uses.
//
// Status is "queued", "in_progress" or "completed"; Conclusion is set only
// once completed and is one of "success", "failure", "cancelled", "timed_out",
// "neutral" or "skipped".
type CheckRun struct {
	Name        string
	HeadSHA     string
	Status      string
	Conclusion  string
	DetailsURL  string
	StartedAt   *time.Time
	CompletedAt *time.Time
	Title       string // output.title (required by GitHub when output is sent)
	Summary     string // output.summary (required when output is sent)
	Text        string // output.text, optional
}

// body renders the check run as the JSON object GitHub expects. Empty fields
// are omitted: GitHub rejects, for instance, an empty conclusion with a 422.
// headSHA is sent only on creation.
func (r CheckRun) body(withHeadSHA bool) map[string]any {
	b := map[string]any{}
	if r.Name != "" {
		b["name"] = r.Name
	}
	if withHeadSHA && r.HeadSHA != "" {
		b["head_sha"] = r.HeadSHA
	}
	if r.Status != "" {
		b["status"] = r.Status
	}
	if r.Conclusion != "" {
		b["conclusion"] = r.Conclusion
	}
	if r.DetailsURL != "" {
		b["details_url"] = r.DetailsURL
	}
	if r.StartedAt != nil {
		b["started_at"] = r.StartedAt.UTC().Format(time.RFC3339)
	}
	if r.CompletedAt != nil {
		b["completed_at"] = r.CompletedAt.UTC().Format(time.RFC3339)
	}
	if r.Title != "" || r.Summary != "" {
		output := map[string]any{
			"title":   r.Title,
			"summary": r.Summary,
		}
		if r.Text != "" {
			output["text"] = r.Text
		}
		b["output"] = output
	}
	return b
}

// cachedToken is one installation access token and the instant it stops being
// usable (its stated expiry, less tokenRenewMargin).
type cachedToken struct {
	token   string
	renewAt time.Time
}

// Client talks to one GitHub API base URL and caches installation tokens.
// It is safe for concurrent use.
type Client struct {
	baseURL string
	http    *http.Client
	now     func() time.Time

	mu     sync.Mutex
	tokens map[string]cachedToken
}

// NewClient returns a Client for baseURL (e.g. DefaultBaseURL); one trailing
// slash is trimmed. A nil httpClient means &http.Client{Timeout: 30s}, and a
// nil now means time.Now.
func NewClient(baseURL string, httpClient *http.Client, now func() time.Time) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if now == nil {
		now = time.Now
	}
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		http:    httpClient,
		now:     now,
		tokens:  map[string]cachedToken{},
	}
}

// tokenResponse is the payload of POST /app/installations/{id}/access_tokens.
type tokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// InstallationToken mints, or returns a cached, installation access token for
// cr. A token is reused until five minutes before its stated expiry.
func (c *Client) InstallationToken(ctx context.Context, cr Credentials) (string, error) {
	key := cr.cacheKey()

	c.mu.Lock()
	cached, ok := c.tokens[key]
	c.mu.Unlock()
	if ok && c.now().Before(cached.renewAt) {
		return cached.token, nil
	}

	jwt, err := SignJWT(cr, c.now())
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", c.baseURL, cr.InstallationID)
	resp, body, err := c.do(ctx, http.MethodPost, url, "Bearer "+jwt, nil)
	if err != nil {
		return "", fmt.Errorf("github: installation token: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("github: installation token: %w", statusError(resp.StatusCode, body))
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("github: installation token: decode response: %w", err)
	}
	if tr.Token == "" {
		return "", fmt.Errorf("github: installation token: response carried no token")
	}

	expiresAt, err := time.Parse(time.RFC3339, tr.ExpiresAt)
	if err != nil {
		return "", fmt.Errorf("github: installation token: parse expires_at: %w", err)
	}

	c.mu.Lock()
	c.tokens[key] = cachedToken{token: tr.Token, renewAt: expiresAt.Add(-tokenRenewMargin)}
	c.mu.Unlock()

	return tr.Token, nil
}

// invalidateToken drops the cached token for cr, so the next call re-mints.
func (c *Client) invalidateToken(cr Credentials) {
	c.mu.Lock()
	delete(c.tokens, cr.cacheKey())
	c.mu.Unlock()
}

// CreateCheckRun creates a check run on repo ("owner/name") and returns its id.
func (c *Client) CreateCheckRun(ctx context.Context, cr Credentials, repo string, run CheckRun) (int64, error) {
	url := fmt.Sprintf("%s/repos/%s/check-runs", c.baseURL, repo)
	body, err := c.checkRunRequest(ctx, cr, http.MethodPost, url, run.body(true), http.StatusCreated)
	if err != nil {
		return 0, fmt.Errorf("github: create check run: %w", err)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		return 0, fmt.Errorf("github: create check run: decode response: %w", err)
	}
	return created.ID, nil
}

// UpdateCheckRun patches an existing check run. head_sha is never sent.
func (c *Client) UpdateCheckRun(ctx context.Context, cr Credentials, repo string, id int64, run CheckRun) error {
	url := fmt.Sprintf("%s/repos/%s/check-runs/%d", c.baseURL, repo, id)
	if _, err := c.checkRunRequest(ctx, cr, http.MethodPatch, url, run.body(false), http.StatusOK); err != nil {
		return fmt.Errorf("github: update check run: %w", err)
	}
	return nil
}

// checkRunRequest sends one authenticated check-run request and returns its
// body. A 401 invalidates the cached installation token.
func (c *Client) checkRunRequest(ctx context.Context, cr Credentials, method, url string, payload map[string]any, wantStatus int) ([]byte, error) {
	token, err := c.InstallationToken(ctx, cr)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode request body: %w", err)
	}
	resp, body, err := c.do(ctx, method, url, "Bearer "+token, encoded)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// The token is spent or was revoked; force a fresh mint next time.
		c.invalidateToken(cr)
	}
	if resp.StatusCode != wantStatus {
		return nil, statusError(resp.StatusCode, body)
	}
	return body, nil
}

// do performs one request with the standard GitHub headers and reads the whole
// response body. The authorization value is never echoed into an error.
func (c *Client) do(ctx context.Context, method, url, authorization string, body []byte) (*http.Response, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, nil, fmt.Errorf("build %s request: %w", method, err)
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Accept", acceptHeader)
	req.Header.Set("X-GitHub-Api-Version", apiVersionHeader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, redactURL(url), err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read response body: %w", err)
	}
	return resp, respBody, nil
}

// redactURL keeps request URLs quotable in errors; GitHub API URLs carry no
// secrets today, but this is the single place to strip one if that changes.
func redactURL(url string) string {
	if i := strings.IndexByte(url, '?'); i >= 0 {
		return url[:i]
	}
	return url
}

// statusError renders a non-expected HTTP status with the leading bytes of the
// response body, which is where GitHub puts the reason.
func statusError(status int, body []byte) error {
	snippet := body
	if len(snippet) > errBodyLimit {
		snippet = snippet[:errBodyLimit]
	}
	return fmt.Errorf("unexpected status %d: %s", status, strings.TrimSpace(string(snippet)))
}
