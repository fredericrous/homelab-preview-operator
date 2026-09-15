package enrichment

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fredericrous/homelab-preview-operator/internal/previewcheck"
)

func TestNew_EmptyURLIsNil(t *testing.T) {
	// A nil enricher is how "no --cve-enrichment-url" reaches the reconciler,
	// which fails the trivy check closed instead of reading an unenriched scan
	// as clean.
	if e := New("   "); e != nil {
		t.Errorf("New(blank) = %v, want nil", e)
	}
	if e := New("http://x/api"); e == nil {
		t.Error("New(url) must not be nil")
	}
}

func TestLookup_HappyPath(t *testing.T) {
	var gotBody request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("request body is not the documented {\"cves\": [...]}: %v (%s)", err, raw)
		}
		writeJSON(w, Response{
			Results:   []previewcheck.CVEIntel{{CVE: "CVE-1", KEV: true, EPSS: 0.4}},
			Unknown:   []string{"CVE-2"},
			FetchedAt: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC),
			KEVTotal:  1200,
			EPSSTotal: 330000,
		})
	}))
	defer srv.Close()

	e := New(srv.URL)
	resp, err := e.Lookup(context.Background(), []string{"CVE-2", "CVE-1"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(gotBody.CVEs) != 2 || gotBody.CVEs[0] != "CVE-1" {
		t.Errorf("request cves = %v, want them sorted", gotBody.CVEs)
	}
	if !resp.Usable() {
		t.Errorf("a fresh, populated cache must be usable: %+v", resp)
	}
	if len(resp.Results) != 1 || resp.Results[0].CVE != "CVE-1" {
		t.Errorf("results = %+v", resp.Results)
	}
	if len(resp.Unknown) != 1 {
		t.Errorf("unknown = %v", resp.Unknown)
	}
}

func TestLookup_EmptySetIsNeverPosted(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	// An empty list earns a 400, which the ladder above reads as terminal — so
	// a perfectly clean image would fail closed. The caller must never get
	// there, and neither must we.
	if _, err := New(srv.URL).Lookup(context.Background(), nil); err == nil {
		t.Fatal("an empty CVE set must be refused locally")
	}
	if IsTerminal(fmt.Errorf("wrapped")) {
		t.Error("a plain error must not read as terminal")
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Errorf("the endpoint was called %d times for an empty set", calls)
	}
}

func TestLookup_4xxIsTerminal_5xxIsNot(t *testing.T) {
	for _, tc := range []struct {
		code     int
		terminal bool
	}{
		{http.StatusBadRequest, true},
		{http.StatusNotFound, true},
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
	} {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte("nope"))
			}))
			defer srv.Close()

			_, err := New(srv.URL).Lookup(context.Background(), []string{"CVE-1"})
			if err == nil {
				t.Fatalf("HTTP %d must be an error", tc.code)
			}
			if IsTerminal(err) != tc.terminal {
				t.Errorf("IsTerminal(HTTP %d) = %v, want %v (%v)", tc.code, IsTerminal(err), tc.terminal, err)
			}
		})
	}
}

func TestLookup_StaleAndEmptyCacheAreNotUsable(t *testing.T) {
	cases := []struct {
		name string
		resp Response
	}{
		{"server says stale", Response{KEVTotal: 1200, Stale: true}},
		// An empty cache answers every id as "unknown", and unknown counts as
		// clean — so a vulnerable image would sail through a perfectly
		// well-formed 200.
		{"empty cache", Response{KEVTotal: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, tc.resp)
			}))
			defer srv.Close()

			resp, err := New(srv.URL).Lookup(context.Background(), []string{"CVE-1"})
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			if resp.Usable() {
				t.Errorf("%+v must not be usable", resp)
			}
		})
	}
}

func TestLookup_ChunksAndMergesConservatively(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		var body request
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		if len(body.CVEs) > maxChunk {
			t.Errorf("chunk of %d ids exceeds %d", len(body.CVEs), maxChunk)
		}
		resp := Response{
			Results:   []previewcheck.CVEIntel{{CVE: body.CVEs[0]}},
			Unknown:   []string{fmt.Sprintf("unknown-%d", n)},
			KEVTotal:  1000 * int(n),
			EPSSTotal: 2000 * int(n),
			FetchedAt: time.Date(2026, 9, int(n)+1, 0, 0, 0, 0, time.UTC),
			Stale:     n == 2,
		}
		writeJSON(w, resp)
	}))
	defer srv.Close()

	ids := make([]string, 0, 450)
	for i := 0; i < 450; i++ {
		ids = append(ids, fmt.Sprintf("CVE-2024-%04d", i))
	}
	resp, err := New(srv.URL).Lookup(context.Background(), ids)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if calls != 3 {
		t.Errorf("450 ids in chunks of %d = %d calls, want 3", maxChunk, calls)
	}
	// Every merged field takes the least confident reading: one stale chunk
	// makes the whole answer stale, the totals take the minimum, fetched_at the
	// oldest, and unknown is the union.
	if !resp.Stale {
		t.Error("one stale chunk must make the aggregate stale")
	}
	if resp.KEVTotal != 1000 || resp.EPSSTotal != 2000 {
		t.Errorf("totals = %d/%d, want the minimum 1000/2000", resp.KEVTotal, resp.EPSSTotal)
	}
	if want := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC); !resp.FetchedAt.Equal(want) {
		t.Errorf("fetchedAt = %s, want the oldest %s", resp.FetchedAt, want)
	}
	if len(resp.Unknown) != 3 || len(resp.Results) != 3 {
		t.Errorf("union = %d unknown / %d results, want 3/3", len(resp.Unknown), len(resp.Results))
	}
}

func TestLookup_MemoisesPerCVESet(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		writeJSON(w, Response{KEVTotal: 10, Results: []previewcheck.CVEIntel{{CVE: "CVE-1"}}})
	}))
	defer srv.Close()

	e := New(srv.URL)
	// The requeue ladder polls a running check every 5-30 s for up to 30 min;
	// without memoisation one image's CVE set would be POSTed dozens of times.
	for i := 0; i < 5; i++ {
		if _, err := e.Lookup(context.Background(), []string{"CVE-1", "CVE-2"}); err != nil {
			t.Fatalf("Lookup: %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (memoised)", calls)
	}
	// Order must not matter: the key is the SORTED set.
	if _, err := e.Lookup(context.Background(), []string{"CVE-2", "CVE-1"}); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if calls != 1 {
		t.Errorf("a re-ordered identical set re-posted: calls = %d", calls)
	}
	// A different set really does go out.
	if _, err := e.Lookup(context.Background(), []string{"CVE-3"}); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if calls != 2 {
		t.Errorf("a new set must be fetched: calls = %d, want 2", calls)
	}

	// Past the TTL the memo lets go.
	e.now = func() time.Time { return time.Now().Add(memoTTL + time.Minute) }
	if _, err := e.Lookup(context.Background(), []string{"CVE-3"}); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if calls != 3 {
		t.Errorf("an expired memo entry must be refetched: calls = %d, want 3", calls)
	}
}

func TestLookup_CancelledContextStopsTheRequest(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	// The per-request deadline rides on the CALLER'S context, so a cancelled
	// reconcile drops the in-flight request instead of holding a worker.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if _, err := New(srv.URL).Lookup(ctx, []string{"CVE-1"}); err == nil {
		t.Fatal("a cancelled context must fail the lookup")
	}
	if elapsed := time.Since(start); elapsed > perRequestTimeout {
		t.Errorf("cancellation took %s, longer than the per-request timeout", elapsed)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestLookup_UnusableAnswersAreNotMemoised(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stale while cluster-vision is restarting, healthy once it is back.
		if atomic.AddInt32(&calls, 1) == 1 {
			writeJSON(w, Response{KEVTotal: 1200, Stale: true})
			return
		}
		writeJSON(w, Response{KEVTotal: 1200, Results: []previewcheck.CVEIntel{{CVE: "CVE-1"}}})
	}))
	defer srv.Close()

	e := New(srv.URL)
	first, err := e.Lookup(context.Background(), []string{"CVE-1"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if first.Usable() {
		t.Fatal("setup: the first answer should be unusable")
	}

	// Memoising "I do not know" for memoTTL would turn a sixty-second blip into
	// fifteen minutes of EnrichmentUnavailable — long enough to eat a
	// thirty-minute run deadline and expire a preview that was judgeable.
	second, err := e.Lookup(context.Background(), []string{"CVE-1"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !second.Usable() {
		t.Error("a stale answer was served from the memo after the endpoint recovered")
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2: the unusable answer must not be cached", calls)
	}

	// The usable one IS memoised.
	if _, err := e.Lookup(context.Background(), []string{"CVE-1"}); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2: a usable answer must be memoised", calls)
	}
}
