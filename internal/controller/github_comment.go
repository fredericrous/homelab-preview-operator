package controller

import (
	"net/http"
	"strings"
)

// githubClient talks to the GitHub issue-comment API.
type githubClient struct {
	restCommentClient
}

// newGitHubClient builds a client for api.github.com, or for a GitHub
// Enterprise API root when baseURL is set.
func newGitHubClient(baseURL, token string) *githubClient {
	if baseURL == "" {
		baseURL = githubAPIBaseURL
	}

	return &githubClient{
		restCommentClient: restCommentClient{
			baseURL:  strings.TrimSuffix(baseURL, "/"),
			token:    token,
			accept:   "application/vnd.github.v3+json",
			provider: "GitHub",
			client:   &http.Client{Timeout: forgeRequestTimeout},
		},
	}
}
