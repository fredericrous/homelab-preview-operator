package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	logr "github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
)

func forgeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}
	if err := sourcev1.AddToScheme(s); err != nil {
		t.Fatalf("adding sourcev1 to scheme: %v", err)
	}
	if err := kustomizev1.AddToScheme(s); err != nil {
		t.Fatalf("adding kustomizev1 to scheme: %v", err)
	}
	return s
}

func TestNewCommentClient(t *testing.T) {
	tests := []struct {
		name     string
		cfg      ForgeConfig
		wantType string
		wantErr  string
	}{
		{"empty provider defaults to github", ForgeConfig{}, "*controller.githubClient", ""},
		{"github", ForgeConfig{Provider: ProviderGitHub}, "*controller.githubClient", ""},
		{"gitea with base URL", ForgeConfig{Provider: ProviderGitea, APIBaseURL: "https://git.daddyshome.fr"}, "*controller.giteaClient", ""},
		{"gitea without base URL", ForgeConfig{Provider: ProviderGitea}, "", "requires an API base URL"},
		{"unknown provider", ForgeConfig{Provider: "bitbucket"}, "", "unsupported git provider"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := newCommentClient(tt.cfg, "token")
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("expected error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("newCommentClient: %v", err)
			}
			if got := typeName(c); got != tt.wantType {
				t.Errorf("expected %s, got %s", tt.wantType, got)
			}
		})
	}
}

func typeName(v any) string {
	switch v.(type) {
	case *githubClient:
		return "*controller.githubClient"
	case *giteaClient:
		return "*controller.giteaClient"
	default:
		return "unknown"
	}
}

func TestAPIBaseURLFromCloneURL(t *testing.T) {
	tests := []struct {
		name     string
		cloneURL string
		expected string
		wantErr  bool
	}{
		{"https with .git", "https://git.daddyshome.fr/fredericrous/homelab.git", "https://git.daddyshome.fr", false},
		{"https without .git", "https://git.daddyshome.fr/fredericrous/homelab", "https://git.daddyshome.fr", false},
		{"https with port", "https://git.daddyshome.fr:3000/fredericrous/homelab.git", "https://git.daddyshome.fr:3000", false},
		{"https with credentials", "https://user:pass@git.daddyshome.fr/fredericrous/homelab.git", "https://git.daddyshome.fr", false},
		{"plain http", "http://gitea.internal/owner/repo.git", "http://gitea.internal", false},
		{"ssh scheme drops port", "ssh://git@git.daddyshome.fr:2222/fredericrous/homelab.git", "https://git.daddyshome.fr", false},
		{"scp-like", "git@git.daddyshome.fr:fredericrous/homelab.git", "https://git.daddyshome.fr", false},
		{"github", "https://github.com/fredericrous/homelab.git", "https://github.com", false},
		{"surrounding whitespace", "  https://git.daddyshome.fr/o/r.git\n", "https://git.daddyshome.fr", false},
		{"empty", "", "", true},
		{"no host", "https:///owner/repo.git", "", true},
		{"not a URL", "just-a-string", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := apiBaseURLFromCloneURL(tt.cloneURL)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("apiBaseURLFromCloneURL(%q): %v", tt.cloneURL, err)
			}
			if got != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, got)
			}
		})
	}
}

func TestGetForgeToken(t *testing.T) {
	tests := []struct {
		name     string
		data     map[string][]byte
		expected string
		wantErr  bool
	}{
		{"password key", map[string][]byte{"password": []byte("gh-token\n")}, "gh-token", false},
		{"token key", map[string][]byte{"token": []byte(" forgejo-token ")}, "forgejo-token", false},
		{"password wins", map[string][]byte{"password": []byte("first"), "token": []byte("second")}, "first", false},
		{"empty password falls back to token", map[string][]byte{"password": []byte(""), "token": []byte("second")}, "second", false},
		{"no usable key", map[string][]byte{"username": []byte("bot")}, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "github-token", Namespace: "preview-pr-7"},
				Data:       tt.data,
			}
			c := fake.NewClientBuilder().WithScheme(forgeScheme(t)).WithObjects(secret).Build()

			token, err := getForgeToken(context.Background(), c, "preview-pr-7")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got token %q", token)
				}
				return
			}
			if err != nil {
				t.Fatalf("getForgeToken: %v", err)
			}
			if token != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, token)
			}
		})
	}
}

func TestGetForgeTokenMissingSecret(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(forgeScheme(t)).Build()

	if _, err := getForgeToken(context.Background(), c, "preview-pr-7"); err == nil {
		t.Fatal("expected an error when the secret is absent")
	}
}

// previewKustomization builds a preview Kustomization pointing at a Flux
// GitRepository source.
func previewKustomization(sourceName, sourceNamespace string) *kustomizev1.Kustomization {
	return &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "preview-pr-7"},
		Spec: kustomizev1.KustomizationSpec{
			SourceRef: kustomizev1.CrossNamespaceSourceReference{
				Kind:      sourcev1.GitRepositoryKind,
				Name:      sourceName,
				Namespace: sourceNamespace,
			},
		},
	}
}

func TestForgeConfigDefaultsToGitHub(t *testing.T) {
	r := &KustomizationReconciler{GitRepo: "fredericrous/homelab"}

	cfg, err := r.forgeConfig(context.Background(), previewKustomization("flux-system", "flux-system"))
	if err != nil {
		t.Fatalf("forgeConfig: %v", err)
	}
	if cfg.Provider != ProviderGitHub {
		t.Errorf("expected provider %q, got %q", ProviderGitHub, cfg.Provider)
	}
	// GitHub resolves its default base URL in the client, not here.
	if cfg.APIBaseURL != "" {
		t.Errorf("expected no base URL, got %q", cfg.APIBaseURL)
	}
	if cfg.Repo != "fredericrous/homelab" {
		t.Errorf("unexpected repo %q", cfg.Repo)
	}
}

func TestForgeConfigGiteaDerivesBaseURLFromSource(t *testing.T) {
	source := &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "flux-system", Namespace: "flux-system"},
		Spec:       sourcev1.GitRepositorySpec{URL: "https://git.daddyshome.fr/fredericrous/homelab.git"},
	}
	c := fake.NewClientBuilder().WithScheme(forgeScheme(t)).WithObjects(source).Build()
	r := &KustomizationReconciler{Client: c, GitRepo: "fredericrous/homelab", GitProvider: ProviderGitea}

	cfg, err := r.forgeConfig(context.Background(), previewKustomization("flux-system", "flux-system"))
	if err != nil {
		t.Fatalf("forgeConfig: %v", err)
	}
	if cfg.APIBaseURL != "https://git.daddyshome.fr" {
		t.Errorf("expected derived base URL, got %q", cfg.APIBaseURL)
	}
}

func TestForgeConfigGiteaSourceRefDefaultsToKustomizationNamespace(t *testing.T) {
	source := &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "app-src", Namespace: "preview-pr-7"},
		Spec:       sourcev1.GitRepositorySpec{URL: "ssh://git@git.daddyshome.fr:2222/fredericrous/homelab.git"},
	}
	c := fake.NewClientBuilder().WithScheme(forgeScheme(t)).WithObjects(source).Build()
	r := &KustomizationReconciler{Client: c, GitRepo: "fredericrous/homelab", GitProvider: ProviderGitea}

	cfg, err := r.forgeConfig(context.Background(), previewKustomization("app-src", ""))
	if err != nil {
		t.Fatalf("forgeConfig: %v", err)
	}
	if cfg.APIBaseURL != "https://git.daddyshome.fr" {
		t.Errorf("expected derived base URL, got %q", cfg.APIBaseURL)
	}
}

func TestForgeConfigExplicitBaseURLSkipsSourceLookup(t *testing.T) {
	// No GitRepository exists: an explicit base URL must short-circuit the lookup.
	c := fake.NewClientBuilder().WithScheme(forgeScheme(t)).Build()
	r := &KustomizationReconciler{
		Client:        c,
		GitRepo:       "fredericrous/homelab",
		GitProvider:   ProviderGitea,
		GitAPIBaseURL: "https://forge.example.com/api/v1",
	}

	cfg, err := r.forgeConfig(context.Background(), previewKustomization("flux-system", "flux-system"))
	if err != nil {
		t.Fatalf("forgeConfig: %v", err)
	}
	if cfg.APIBaseURL != "https://forge.example.com/api/v1" {
		t.Errorf("expected the configured base URL, got %q", cfg.APIBaseURL)
	}
}

func TestForgeConfigGiteaMissingSource(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(forgeScheme(t)).Build()
	r := &KustomizationReconciler{Client: c, GitRepo: "fredericrous/homelab", GitProvider: ProviderGitea}

	if _, err := r.forgeConfig(context.Background(), previewKustomization("flux-system", "flux-system")); err == nil {
		t.Fatal("expected an error when the GitRepository is absent")
	}
}

func TestForgeConfigGiteaNonGitSource(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(forgeScheme(t)).Build()
	r := &KustomizationReconciler{Client: c, GitRepo: "fredericrous/homelab", GitProvider: ProviderGitea}

	ks := previewKustomization("charts", "flux-system")
	ks.Spec.SourceRef.Kind = "OCIRepository"

	if _, err := r.forgeConfig(context.Background(), ks); err == nil {
		t.Fatal("expected an error for a non-GitRepository source")
	}
}

func TestPostPRCommentCreatesOnForgejo(t *testing.T) {
	var createdPath string
	var createdBody map[string]string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]forgeComment{{ID: 1, Body: "unrelated"}})
		case http.MethodPost:
			createdPath = r.URL.Path
			json.NewDecoder(r.Body).Decode(&createdBody)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id": 2}`))
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "github-token", Namespace: "preview-pr-7"},
		Data:       map[string][]byte{"token": []byte("forgejo-token")},
	}
	c := fake.NewClientBuilder().WithScheme(forgeScheme(t)).WithObjects(secret).Build()

	forge := ForgeConfig{Provider: ProviderGitea, APIBaseURL: server.URL, Repo: "fredericrous/homelab"}
	err := PostPRComment(context.Background(), c, "preview-pr-7", forge, "7", "myapp", "https://pr-7-myapp.daddyshome.fr", logr.Discard())
	if err != nil {
		t.Fatalf("PostPRComment: %v", err)
	}

	if createdPath != "/api/v1/repos/fredericrous/homelab/issues/7/comments" {
		t.Errorf("unexpected create path: %s", createdPath)
	}
	if !strings.Contains(createdBody["body"], "https://pr-7-myapp.daddyshome.fr") {
		t.Errorf("comment body missing preview URL: %q", createdBody["body"])
	}
}

func TestPostPRCommentUpdatesExistingOnForgejo(t *testing.T) {
	var updatedPath string
	var updatedBody map[string]string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]forgeComment{{ID: 42, Body: commentMarker + "\nstale"}})
		case http.MethodPatch:
			updatedPath = r.URL.Path
			json.NewDecoder(r.Body).Decode(&updatedBody)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id": 42}`))
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "github-token", Namespace: "preview-pr-7"},
		Data:       map[string][]byte{"password": []byte("forgejo-token")},
	}
	c := fake.NewClientBuilder().WithScheme(forgeScheme(t)).WithObjects(secret).Build()

	forge := ForgeConfig{Provider: ProviderGitea, APIBaseURL: server.URL, Repo: "fredericrous/homelab"}
	err := PostPRComment(context.Background(), c, "preview-pr-7", forge, "7", "myapp", "https://pr-7-myapp.daddyshome.fr", logr.Discard())
	if err != nil {
		t.Fatalf("PostPRComment: %v", err)
	}

	if updatedPath != "/api/v1/repos/fredericrous/homelab/issues/comments/42" {
		t.Errorf("unexpected update path: %s", updatedPath)
	}
	if !strings.Contains(updatedBody["body"], "https://pr-7-myapp.daddyshome.fr") {
		t.Errorf("updated body missing preview URL: %q", updatedBody["body"])
	}
}

func TestPostPRCommentRejectsUnknownProvider(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "github-token", Namespace: "preview-pr-7"},
		Data:       map[string][]byte{"password": []byte("t")},
	}
	c := fake.NewClientBuilder().WithScheme(forgeScheme(t)).WithObjects(secret).Build()

	forge := ForgeConfig{Provider: "svn", Repo: "owner/repo"}
	err := PostPRComment(context.Background(), c, "preview-pr-7", forge, "7", "myapp", "https://example.test", logr.Discard())
	if err == nil {
		t.Fatal("expected an error for an unsupported provider")
	}
}
