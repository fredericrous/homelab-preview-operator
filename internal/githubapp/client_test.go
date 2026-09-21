package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testInstallationID = "42"
	testRepo           = "o/r"
	tokenPath          = "/app/installations/42/access_tokens"
	checkRunsPath      = "/repos/o/r/check-runs"
)

var baseTime = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

// testCredentials returns credentials backed by a freshly generated key.
func testCredentials(t *testing.T) Credentials {
	t.Helper()
	_, pkcs1PEM, _ := testKey(t)
	return Credentials{
		AppID:          "123456",
		InstallationID: testInstallationID,
		PrivateKeyPEM:  pkcs1PEM,
	}
}

// clock is a settable time source injected into the Client.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *clock { return &clock{t: t} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// assertHeaders checks the headers every GitHub call must carry.
func assertHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Accept"); got != acceptHeader {
		t.Errorf("%s Accept = %q, want %q", r.URL.Path, got, acceptHeader)
	}
	if got := r.Header.Get("X-GitHub-Api-Version"); got != apiVersionHeader {
		t.Errorf("%s X-GitHub-Api-Version = %q, want %q", r.URL.Path, got, apiVersionHeader)
	}
}

// bearer returns the token part of the Authorization header.
func bearer(t *testing.T, r *http.Request) string {
	t.Helper()
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		t.Errorf("%s Authorization = %q, want a Bearer credential", r.URL.Path, auth)
		return ""
	}
	return strings.TrimPrefix(auth, "Bearer ")
}

// decodeBody reads a request body as a JSON object.
func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal body %q: %v", raw, err)
	}
	return body
}

func TestInstallationTokenCached(t *testing.T) {
	cr := testCredentials(t)
	clk := newClock(baseTime)
	expiresAt := baseTime.Add(time.Hour)

	var mints int32
	var checkRunTokens []string
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertHeaders(t, r)
		switch r.URL.Path {
		case tokenPath:
			if r.Method != http.MethodPost {
				t.Errorf("token call method = %s, want POST", r.Method)
			}
			jwt := bearer(t, r)
			if n := len(strings.Split(jwt, ".")); n != 3 {
				t.Errorf("token call carried %d JWT segments, want 3: %q", n, jwt)
			}
			n := atomic.AddInt32(&mints, 1)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`,
				fmt.Sprintf("ghs-token-%d", n), expiresAt.Format(time.RFC3339))
		case checkRunsPath:
			mu.Lock()
			checkRunTokens = append(checkRunTokens, bearer(t, r))
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id": 1}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, srv.Client(), clk.now)
	ctx := context.Background()
	run := CheckRun{Name: "preview", HeadSHA: "deadbeef", Status: "queued"}

	for i := 0; i < 2; i++ {
		if _, err := c.CreateCheckRun(ctx, cr, testRepo, run); err != nil {
			t.Fatalf("CreateCheckRun #%d: %v", i+1, err)
		}
	}
	if got := atomic.LoadInt32(&mints); got != 1 {
		t.Fatalf("minted %d tokens for two calls, want 1", got)
	}

	// Past expires_at minus five minutes, the cached token is spent.
	clk.set(expiresAt.Add(-4 * time.Minute))
	if _, err := c.CreateCheckRun(ctx, cr, testRepo, run); err != nil {
		t.Fatalf("CreateCheckRun after expiry: %v", err)
	}
	if got := atomic.LoadInt32(&mints); got != 2 {
		t.Fatalf("minted %d tokens after expiry, want 2", got)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"ghs-token-1", "ghs-token-1", "ghs-token-2"}
	if len(checkRunTokens) != len(want) {
		t.Fatalf("check-run tokens = %v, want %v", checkRunTokens, want)
	}
	for i := range want {
		if checkRunTokens[i] != want[i] {
			t.Errorf("check-run call %d used token %q, want %q", i+1, checkRunTokens[i], want[i])
		}
	}
}

// tokenServer serves the access-token endpoint and delegates everything else
// to next, counting token mints.
func tokenServer(t *testing.T, mints *int32, next http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertHeaders(t, r)
		if r.URL.Path == tokenPath {
			atomic.AddInt32(mints, 1)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"ghs-token","expires_at":%q}`,
				baseTime.Add(time.Hour).Format(time.RFC3339))
			return
		}
		next(w, r)
	}))
}

func TestCreateCheckRunBody(t *testing.T) {
	cr := testCredentials(t)
	started := baseTime.Add(-2 * time.Minute)

	var mints int32
	var got map[string]any
	srv := tokenServer(t, &mints, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != checkRunsPath {
			t.Errorf("got %s %s, want POST %s", r.Method, r.URL.Path, checkRunsPath)
		}
		got = decodeBody(t, r)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id": 123}`)
	})
	defer srv.Close()

	c := NewClient(srv.URL, srv.Client(), newClock(baseTime).now)
	id, err := c.CreateCheckRun(context.Background(), cr, testRepo, CheckRun{
		Name:       "preview/migration",
		HeadSHA:    "cafebabe",
		Status:     "in_progress",
		DetailsURL: "https://example.test/run/1",
		StartedAt:  &started,
		Title:      "Migration check",
		Summary:    "Running",
	})
	if err != nil {
		t.Fatalf("CreateCheckRun: %v", err)
	}
	if id != 123 {
		t.Fatalf("id = %d, want 123", id)
	}

	if got["name"] != "preview/migration" {
		t.Errorf("name = %v", got["name"])
	}
	if got["head_sha"] != "cafebabe" {
		t.Errorf("head_sha = %v", got["head_sha"])
	}
	if got["status"] != "in_progress" {
		t.Errorf("status = %v", got["status"])
	}
	if got["details_url"] != "https://example.test/run/1" {
		t.Errorf("details_url = %v", got["details_url"])
	}
	if got["started_at"] != started.UTC().Format(time.RFC3339) {
		t.Errorf("started_at = %v", got["started_at"])
	}
	if _, ok := got["conclusion"]; ok {
		t.Errorf("conclusion must be absent when empty, body = %v", got)
	}
	if _, ok := got["completed_at"]; ok {
		t.Errorf("completed_at must be absent when nil, body = %v", got)
	}

	output, ok := got["output"].(map[string]any)
	if !ok {
		t.Fatalf("output = %v, want an object", got["output"])
	}
	if output["title"] != "Migration check" {
		t.Errorf("output.title = %v", output["title"])
	}
	if output["summary"] != "Running" {
		t.Errorf("output.summary = %v", output["summary"])
	}
	if _, ok := output["text"]; ok {
		t.Errorf("output.text must be absent when empty, output = %v", output)
	}
}

func TestCreateCheckRunOmitsEmptyOutput(t *testing.T) {
	cr := testCredentials(t)

	var mints int32
	var got map[string]any
	srv := tokenServer(t, &mints, func(w http.ResponseWriter, r *http.Request) {
		got = decodeBody(t, r)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id": 7}`)
	})
	defer srv.Close()

	c := NewClient(srv.URL, srv.Client(), newClock(baseTime).now)
	if _, err := c.CreateCheckRun(context.Background(), cr, testRepo, CheckRun{
		Name: "preview", HeadSHA: "abc", Status: "queued",
	}); err != nil {
		t.Fatalf("CreateCheckRun: %v", err)
	}
	if _, ok := got["output"]; ok {
		t.Errorf("output must be absent when title and summary are empty, body = %v", got)
	}
}

func TestUpdateCheckRunBody(t *testing.T) {
	cr := testCredentials(t)
	completed := baseTime

	var mints int32
	var got map[string]any
	var path string
	srv := tokenServer(t, &mints, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		path = r.URL.Path
		got = decodeBody(t, r)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id": 123}`)
	})
	defer srv.Close()

	c := NewClient(srv.URL, srv.Client(), newClock(baseTime).now)
	err := c.UpdateCheckRun(context.Background(), cr, testRepo, 123, CheckRun{
		HeadSHA:     "cafebabe", // must NOT be sent on PATCH
		Status:      "completed",
		Conclusion:  "failure",
		CompletedAt: &completed,
		Title:       "Migration check",
		Summary:     "A migration is not reversible",
		Text:        "details",
	})
	if err != nil {
		t.Fatalf("UpdateCheckRun: %v", err)
	}

	if want := "/repos/o/r/check-runs/123"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if got["status"] != "completed" {
		t.Errorf("status = %v", got["status"])
	}
	if got["conclusion"] != "failure" {
		t.Errorf("conclusion = %v", got["conclusion"])
	}
	if got["completed_at"] != completed.UTC().Format(time.RFC3339) {
		t.Errorf("completed_at = %v", got["completed_at"])
	}
	if _, ok := got["head_sha"]; ok {
		t.Errorf("head_sha must never be sent on PATCH, body = %v", got)
	}
	if _, ok := got["name"]; ok {
		t.Errorf("name must be absent when empty, body = %v", got)
	}
	output, ok := got["output"].(map[string]any)
	if !ok {
		t.Fatalf("output = %v, want an object", got["output"])
	}
	if output["text"] != "details" {
		t.Errorf("output.text = %v", output["text"])
	}
}

func TestNon2xxSurfaces(t *testing.T) {
	cr := testCredentials(t)
	const errBody = `{"message":"Invalid request.","errors":[{"resource":"CheckRun","field":"conclusion"}]}`

	var mints int32
	srv := tokenServer(t, &mints, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprint(w, errBody)
	})
	defer srv.Close()

	c := NewClient(srv.URL, srv.Client(), newClock(baseTime).now)
	_, err := c.CreateCheckRun(context.Background(), cr, testRepo, CheckRun{
		Name: "preview", HeadSHA: "abc", Status: "completed",
	})
	if err == nil {
		t.Fatal("want an error for a 422, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "422") {
		t.Errorf("error %q does not mention the status code", msg)
	}
	if !strings.Contains(msg, "Invalid request.") {
		t.Errorf("error %q does not carry the response body", msg)
	}
	if !strings.Contains(msg, "create check run") {
		t.Errorf("error %q is not wrapped with the operation", msg)
	}
	if strings.Contains(msg, "ghs-token") || strings.Contains(msg, "PRIVATE KEY") {
		t.Errorf("error %q leaks a credential", msg)
	}
}

func TestErrorBodyTruncated(t *testing.T) {
	cr := testCredentials(t)
	long := strings.Repeat("x", 2000)

	var mints int32
	srv := tokenServer(t, &mints, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, long)
	})
	defer srv.Close()

	c := NewClient(srv.URL, srv.Client(), newClock(baseTime).now)
	_, err := c.CreateCheckRun(context.Background(), cr, testRepo, CheckRun{Name: "n", HeadSHA: "s", Status: "queued"})
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, strings.Repeat("x", errBodyLimit)) {
		t.Errorf("error dropped body bytes, want %d: %q", errBodyLimit, msg)
	}
	if strings.Contains(msg, strings.Repeat("x", errBodyLimit+1)) {
		t.Errorf("error carried more than %d body bytes: %q", errBodyLimit, msg)
	}
}

func Test401InvalidatesCache(t *testing.T) {
	cr := testCredentials(t)

	var mints int32
	var checkRuns int32
	srv := tokenServer(t, &mints, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&checkRuns, 1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"Bad credentials"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id": 5}`)
	})
	defer srv.Close()

	c := NewClient(srv.URL, srv.Client(), newClock(baseTime).now)
	ctx := context.Background()
	run := CheckRun{Name: "preview", HeadSHA: "abc", Status: "queued"}

	if _, err := c.CreateCheckRun(ctx, cr, testRepo, run); err == nil {
		t.Fatal("want an error for a 401, got nil")
	}
	if got := atomic.LoadInt32(&mints); got != 1 {
		t.Fatalf("minted %d tokens before the 401 retry, want 1", got)
	}

	id, err := c.CreateCheckRun(ctx, cr, testRepo, run)
	if err != nil {
		t.Fatalf("CreateCheckRun after 401: %v", err)
	}
	if id != 5 {
		t.Errorf("id = %d, want 5", id)
	}
	if got := atomic.LoadInt32(&mints); got != 2 {
		t.Fatalf("minted %d tokens after a 401, want 2 (cache was not invalidated)", got)
	}
}

func TestNewClientDefaults(t *testing.T) {
	c := NewClient(DefaultBaseURL+"/", nil, nil)
	if c.baseURL != DefaultBaseURL {
		t.Errorf("baseURL = %q, want %q", c.baseURL, DefaultBaseURL)
	}
	if c.http == nil || c.http.Timeout != 30*time.Second {
		t.Errorf("http client = %+v, want a 30s timeout", c.http)
	}
	if c.now == nil {
		t.Error("now must default to time.Now")
	}
}

func TestInstallationTokenConcurrent(t *testing.T) {
	cr := testCredentials(t)

	var mints int32
	srv := tokenServer(t, &mints, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id": 1}`)
	})
	defer srv.Close()

	c := NewClient(srv.URL, srv.Client(), newClock(baseTime).now)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.InstallationToken(context.Background(), cr); err != nil {
				t.Errorf("InstallationToken: %v", err)
			}
		}()
	}
	wg.Wait()
	// The cache does not hold a lock across the HTTP call, so a racing mint
	// is allowed; what matters is that it never deadlocks or corrupts.
	if got, err := c.InstallationToken(context.Background(), cr); err != nil || got != "ghs-token" {
		t.Fatalf("InstallationToken = %q, %v", got, err)
	}
}
