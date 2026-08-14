package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildCommentBody(t *testing.T) {
	body := buildCommentBody("kb-vision", "20", "https://pr-20-kb-vision.daddyshome.fr")

	if !strings.Contains(body, commentMarker) {
		t.Error("comment body missing HTML marker")
	}
	if !strings.Contains(body, "`kb-vision`") {
		t.Error("comment body missing app name")
	}
	if !strings.Contains(body, "#20") {
		t.Error("comment body missing PR number")
	}
	if !strings.Contains(body, "https://pr-20-kb-vision.daddyshome.fr") {
		t.Error("comment body missing preview URL")
	}
	if !strings.Contains(body, "Preview Environment Ready") {
		t.Error("comment body missing header")
	}
	if !strings.Contains(body, "homelab-preview-operator") {
		t.Error("comment body missing operator attribution")
	}
}

func TestBuildCommentBodyContainsClickableLink(t *testing.T) {
	url := "https://pr-5-myapp.daddyshome.fr"
	body := buildCommentBody("myapp", "5", url)

	expectedLink := "[" + url + "](" + url + ")"
	if !strings.Contains(body, expectedLink) {
		t.Errorf("expected clickable markdown link %q in body", expectedLink)
	}
}

func TestGitHubDefaultBaseURL(t *testing.T) {
	c := newGitHubClient("", "test-token")
	if c.baseURL != githubAPIBaseURL {
		t.Errorf("expected default base URL %q, got %q", githubAPIBaseURL, c.baseURL)
	}

	c = newGitHubClient("https://github.example.com/api/v3/", "test-token")
	if c.baseURL != "https://github.example.com/api/v3" {
		t.Errorf("expected trailing slash trimmed, got %q", c.baseURL)
	}
}

func TestCreateComment(t *testing.T) {
	var receivedBody map[string]string
	var receivedAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/repos/owner/repo/issues/42/comments") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		receivedAuth = r.Header.Get("Authorization")

		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}

		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id": 1}`))
	}))
	defer server.Close()

	err := newGitHubClient(server.URL, "test-token").CreateComment(context.Background(), "owner/repo", "42", "test body")
	if err != nil {
		t.Fatalf("createComment failed: %v", err)
	}

	if receivedAuth != "token test-token" {
		t.Errorf("expected auth 'token test-token', got %q", receivedAuth)
	}
	if receivedBody["body"] != "test body" {
		t.Errorf("expected body 'test body', got %q", receivedBody["body"])
	}
}

func TestCreateCommentErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message": "forbidden"}`))
	}))
	defer server.Close()

	err := newGitHubClient(server.URL, "bad-token").CreateComment(context.Background(), "owner/repo", "42", "test")
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should mention status code: %v", err)
	}
	if !strings.Contains(err.Error(), "GitHub") {
		t.Errorf("error should name the provider: %v", err)
	}
}

func TestFindExistingComment(t *testing.T) {
	comments := []forgeComment{
		{ID: 1, Body: "some other comment"},
		{ID: 2, Body: commentMarker + "\n## Preview Ready"},
		{ID: 3, Body: "another comment"},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(comments)
	}))
	defer server.Close()

	found, err := newGitHubClient(server.URL, "test-token").FindComment(context.Background(), "owner/repo", "10")
	if err != nil {
		t.Fatalf("findExistingComment failed: %v", err)
	}
	if found == nil {
		t.Fatal("expected to find existing comment")
	}
	if found.ID != 2 {
		t.Errorf("expected comment ID 2, got %d", found.ID)
	}
}

func TestFindExistingCommentNotFound(t *testing.T) {
	comments := []forgeComment{
		{ID: 1, Body: "unrelated comment"},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(comments)
	}))
	defer server.Close()

	found, err := newGitHubClient(server.URL, "test-token").FindComment(context.Background(), "owner/repo", "10")
	if err != nil {
		t.Fatalf("findExistingComment failed: %v", err)
	}
	if found != nil {
		t.Errorf("expected nil, got comment ID %d", found.ID)
	}
}

func TestUpdateComment(t *testing.T) {
	var receivedBody map[string]string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PATCH" {
			t.Errorf("expected PATCH, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/repos/owner/repo/issues/comments/99") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id": 99}`))
	}))
	defer server.Close()

	err := newGitHubClient(server.URL, "test-token").UpdateComment(context.Background(), "owner/repo", 99, "updated body")
	if err != nil {
		t.Fatalf("updateComment failed: %v", err)
	}
	if receivedBody["body"] != "updated body" {
		t.Errorf("expected 'updated body', got %q", receivedBody["body"])
	}
}
