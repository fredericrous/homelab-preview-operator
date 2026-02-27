package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	commentMarker    = "<!-- preview-operator-comment -->"
	githubAPIBaseURL = "https://api.github.com"
)

// githubComment represents a GitHub issue/PR comment
type githubComment struct {
	ID   int    `json:"id"`
	Body string `json:"body"`
}

// PostPRComment posts or updates a PR comment with the preview URL.
// It uses the GitHub API base URL https://api.github.com.
func PostPRComment(ctx context.Context, k8sClient client.Client, namespace, repo, prNumber, appName, previewURL string, log logr.Logger) error {
	return postPRCommentWithURL(ctx, k8sClient, namespace, repo, prNumber, appName, previewURL, githubAPIBaseURL, log)
}

// postPRCommentWithURL is the testable variant that accepts a custom GitHub API base URL.
func postPRCommentWithURL(ctx context.Context, k8sClient client.Client, namespace, repo, prNumber, appName, previewURL, apiBaseURL string, log logr.Logger) error {
	token, err := getGitHubToken(ctx, k8sClient, namespace)
	if err != nil {
		return fmt.Errorf("getting GitHub token: %w", err)
	}

	body := buildCommentBody(appName, prNumber, previewURL)

	existing, err := findExistingCommentWithURL(token, repo, prNumber, apiBaseURL)
	if err != nil {
		return fmt.Errorf("finding existing comment: %w", err)
	}

	if existing != nil {
		log.Info("Updating existing PR comment", "pr", prNumber, "commentID", existing.ID)
		return updateCommentWithURL(token, repo, existing.ID, body, apiBaseURL)
	}

	log.Info("Creating new PR comment", "pr", prNumber)
	return createCommentWithURL(token, repo, prNumber, body, apiBaseURL)
}

// getGitHubToken reads the GitHub token from the github-token secret in the given namespace.
func getGitHubToken(ctx context.Context, k8sClient client.Client, namespace string) (string, error) {
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: namespace, Name: "github-token"}
	if err := k8sClient.Get(ctx, key, &secret); err != nil {
		return "", fmt.Errorf("reading github-token secret: %w", err)
	}

	token, ok := secret.Data["password"]
	if !ok || len(token) == 0 {
		return "", fmt.Errorf("github-token secret missing 'password' key")
	}

	return strings.TrimSpace(string(token)), nil
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

// findExistingComment searches for a comment with our marker on the given PR.
func findExistingComment(token, repo, prNumber string) (*githubComment, error) {
	return findExistingCommentWithURL(token, repo, prNumber, githubAPIBaseURL)
}

// findExistingCommentWithURL is the testable variant that accepts a custom GitHub API base URL.
func findExistingCommentWithURL(token, repo, prNumber, apiBaseURL string) (*githubComment, error) {
	url := fmt.Sprintf("%s/repos/%s/issues/%s/comments", apiBaseURL, repo, prNumber)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing comments: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(body))
	}

	var comments []githubComment
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

// createComment posts a new comment on the given PR.
func createComment(token, repo, prNumber, body string) error {
	return createCommentWithURL(token, repo, prNumber, body, githubAPIBaseURL)
}

// createCommentWithURL is the testable variant that accepts a custom GitHub API base URL.
func createCommentWithURL(token, repo, prNumber, body, apiBaseURL string) error {
	url := fmt.Sprintf("%s/repos/%s/issues/%s/comments", apiBaseURL, repo, prNumber)

	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", url, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("creating comment: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// updateComment updates an existing comment on the given repo.
func updateComment(token, repo string, commentID int, body string) error {
	return updateCommentWithURL(token, repo, commentID, body, githubAPIBaseURL)
}

// updateCommentWithURL is the testable variant that accepts a custom GitHub API base URL.
func updateCommentWithURL(token, repo string, commentID int, body, apiBaseURL string) error {
	url := fmt.Sprintf("%s/repos/%s/issues/comments/%d", apiBaseURL, repo, commentID)

	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return err
	}

	req, err := http.NewRequest("PATCH", url, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("updating comment: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}
