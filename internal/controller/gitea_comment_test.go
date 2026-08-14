package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGiteaBaseURLGetsAPIPrefix(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		expected string
	}{
		{"instance root", "https://git.daddyshome.fr", "https://git.daddyshome.fr/api/v1"},
		{"trailing slash", "https://git.daddyshome.fr/", "https://git.daddyshome.fr/api/v1"},
		{"already an API root", "https://git.daddyshome.fr/api/v1", "https://git.daddyshome.fr/api/v1"},
		{"API root with slash", "https://git.daddyshome.fr/api/v1/", "https://git.daddyshome.fr/api/v1"},
		{"surrounding whitespace", "  https://git.daddyshome.fr  ", "https://git.daddyshome.fr/api/v1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := newGiteaClient(tt.baseURL, "t").baseURL; got != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, got)
			}
		})
	}
}

func TestGiteaCreateComment(t *testing.T) {
	var receivedBody map[string]string
	var receivedAuth, receivedPath, receivedContentType string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		receivedPath = r.URL.Path
		receivedAuth = r.Header.Get("Authorization")
		receivedContentType = r.Header.Get("Content-Type")

		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}

		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id": 7}`))
	}))
	defer server.Close()

	err := newGiteaClient(server.URL, "forgejo-token").CreateComment(context.Background(), "fredericrous/homelab", "42", "test body")
	if err != nil {
		t.Fatalf("CreateComment failed: %v", err)
	}

	if receivedPath != "/api/v1/repos/fredericrous/homelab/issues/42/comments" {
		t.Errorf("unexpected path: %s", receivedPath)
	}
	if receivedAuth != "token forgejo-token" {
		t.Errorf("expected auth 'token forgejo-token', got %q", receivedAuth)
	}
	if receivedContentType != "application/json" {
		t.Errorf("expected JSON content type, got %q", receivedContentType)
	}
	if receivedBody["body"] != "test body" {
		t.Errorf("expected body 'test body', got %q", receivedBody["body"])
	}
}

func TestGiteaCreateCommentErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message": "token required"}`))
	}))
	defer server.Close()

	err := newGiteaClient(server.URL, "bad-token").CreateComment(context.Background(), "owner/repo", "42", "test")
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention status code: %v", err)
	}
	if !strings.Contains(err.Error(), "Gitea/Forgejo") {
		t.Errorf("error should name the provider: %v", err)
	}
}

func TestGiteaFindComment(t *testing.T) {
	comments := []forgeComment{
		{ID: 11, Body: "a human comment"},
		{ID: 12, Body: commentMarker + "\n## Preview Environment Ready"},
	}

	var receivedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		receivedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(comments)
	}))
	defer server.Close()

	found, err := newGiteaClient(server.URL, "forgejo-token").FindComment(context.Background(), "owner/repo", "10")
	if err != nil {
		t.Fatalf("FindComment failed: %v", err)
	}
	if receivedPath != "/api/v1/repos/owner/repo/issues/10/comments" {
		t.Errorf("unexpected path: %s", receivedPath)
	}
	if found == nil {
		t.Fatal("expected to find the operator comment")
	}
	if found.ID != 12 {
		t.Errorf("expected comment ID 12, got %d", found.ID)
	}
}

func TestGiteaFindCommentNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]forgeComment{{ID: 1, Body: "unrelated"}})
	}))
	defer server.Close()

	found, err := newGiteaClient(server.URL, "forgejo-token").FindComment(context.Background(), "owner/repo", "10")
	if err != nil {
		t.Fatalf("FindComment failed: %v", err)
	}
	if found != nil {
		t.Errorf("expected nil, got comment ID %d", found.ID)
	}
}

func TestGiteaUpdateComment(t *testing.T) {
	var receivedBody map[string]string
	var receivedPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PATCH" {
			t.Errorf("expected PATCH, got %s", r.Method)
		}
		receivedPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id": 99}`))
	}))
	defer server.Close()

	err := newGiteaClient(server.URL, "forgejo-token").UpdateComment(context.Background(), "owner/repo", 99, "updated body")
	if err != nil {
		t.Fatalf("UpdateComment failed: %v", err)
	}
	if receivedPath != "/api/v1/repos/owner/repo/issues/comments/99" {
		t.Errorf("unexpected path: %s", receivedPath)
	}
	if receivedBody["body"] != "updated body" {
		t.Errorf("expected 'updated body', got %q", receivedBody["body"])
	}
}
