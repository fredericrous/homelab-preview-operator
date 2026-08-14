package controller

import (
	"net/http"
	"strings"
)

// giteaClient talks to the Gitea issue-comment API, which self-hosted Forgejo
// serves as well.
type giteaClient struct {
	restCommentClient
}

// newGiteaClient builds a client for a Gitea/Forgejo instance. baseURL may be
// either the instance root (https://git.example.com) or an API root that
// already carries the /api/v1 prefix — both end up at the same routes.
func newGiteaClient(baseURL, token string) *giteaClient {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if !strings.HasSuffix(base, giteaAPIPath) {
		base += giteaAPIPath
	}

	return &giteaClient{
		restCommentClient: restCommentClient{
			baseURL:  base,
			token:    token,
			accept:   "application/json",
			provider: "Gitea/Forgejo",
			client:   &http.Client{Timeout: forgeRequestTimeout},
		},
	}
}
