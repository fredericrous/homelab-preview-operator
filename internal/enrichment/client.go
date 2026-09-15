// Package enrichment talks to cluster-vision's KEV/EPSS endpoint.
//
// It is plain net/http on purpose: the whole client is one POST, and keeping it
// free of controller-runtime makes it testable against httptest rather than
// against a fake API server.
package enrichment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fredericrous/homelab-preview-operator/internal/previewcheck"
)

const (
	// maxChunk is the largest CVE list sent in one request. The endpoint rejects
	// more than 500; 200 keeps a request comfortably inside that and inside the
	// per-request timeout even on a cold cache.
	maxChunk = 200

	// perRequestTimeout bounds a single POST.
	perRequestTimeout = 5 * time.Second

	// TotalBudget bounds every request a single reconcile makes. A reconcile
	// that spends longer than this on enrichment is starving the other three
	// concurrent workers, and the ladder above it is designed to requeue.
	TotalBudget = 10 * time.Second

	// maxBody caps the response we will read. The envelope is bounded by the
	// number of ids we sent, so anything larger is a misrouted request.
	maxBody = 8 << 20

	// memoTTL is how long a per-CVE-set answer is reused. It exists so the
	// requeue ladder (a check that polls every 5-30 s for up to 30 min) does not
	// re-POST the same image's CVE set dozens of times; it is deliberately far
	// shorter than the enrichment data's own 24 h refresh.
	memoTTL = 15 * time.Minute

	// memoMax bounds the memo table.
	memoMax = 128
)

// Response is the aggregated answer for one CVE set.
type Response struct {
	// Results carries the KNOWN CVEs only.
	Results []previewcheck.CVEIntel `json:"results"`
	// Unknown lists the ids the intelligence has never seen. They count as
	// clean — but only because Stale and KEVTotal prove the cache was loaded.
	Unknown []string `json:"unknown"`
	// FetchedAt is when the intelligence was last refreshed from upstream.
	FetchedAt time.Time `json:"fetched_at"`
	// KEVTotal is how many KEV entries the cache holds overall. Zero means the
	// cache is empty, which would otherwise read as "nothing is exploited".
	KEVTotal int `json:"kev_total"`
	// EPSSTotal is how many EPSS entries the cache holds overall.
	EPSSTotal int `json:"epss_total"`
	// Stale is the server's own judgement that its data is too old to trust.
	Stale bool `json:"stale"`
}

// Usable reports whether this answer may be turned into a verdict at all.
//
// An empty cache and a stale cache are the two ways a perfectly well-formed
// 200 response means "I do not know": every CVE comes back unknown, every
// unknown counts as clean, and a vulnerable image sails through. Both are
// treated as unavailability, never as a pass.
func (r Response) Usable() bool {
	return !r.Stale && r.KEVTotal > 0
}

// CVEEnricher answers "how exploitable are these CVEs?".
//
// It is an interface so the trivy check can be tested without a server, and so
// a nil enricher (no --cve-enrichment-url) is a distinguishable state rather
// than a client pointed at the empty string.
type CVEEnricher interface {
	Lookup(ctx context.Context, cves []string) (Response, error)
}

// StatusError is a non-2xx answer. Its Terminal method is what separates "try
// again in 30 s" from "this request will never be accepted".
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("cve enrichment: HTTP %d: %s", e.Code, e.Body)
}

// Terminal reports whether retrying is pointless. A 4xx means we sent something
// the endpoint refuses — an over-long list, a malformed id, an empty set — and
// the very same body would be refused again in 30 seconds.
func (e *StatusError) Terminal() bool {
	return e.Code >= 400 && e.Code < 500
}

// IsTerminal reports whether an error from Lookup is permanent for this input.
func IsTerminal(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Terminal()
	}
	return false
}

type memoEntry struct {
	resp Response
	at   time.Time
}

// ClusterVisionEnricher is the CVEEnricher backed by cluster-vision's
// in-memory KEV/EPSS cache.
type ClusterVisionEnricher struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu   sync.Mutex
	memo map[string]memoEntry
}

// New returns an enricher posting to url, or nil when url is empty.
//
// A nil enricher is meaningful: the trivy check fails closed rather than
// guessing, so an operator who forgets the flag gets inconclusive verdicts
// instead of falsely clean ones.
func New(url string) *ClusterVisionEnricher {
	if strings.TrimSpace(url) == "" {
		return nil
	}
	return &ClusterVisionEnricher{
		url: url,
		// No Client.Timeout: the deadline rides on the request context so the
		// reconcile's own cancellation actually cancels the in-flight request,
		// which a transport-level timeout would not.
		client: &http.Client{},
		now:    time.Now,
		memo:   map[string]memoEntry{},
	}
}

type request struct {
	CVEs []string `json:"cves"`
}

// Lookup enriches a CVE set, chunked and memoised.
//
// The whole call is bounded by TotalBudget derived FROM THE CALLER'S CONTEXT,
// and each chunk by perRequestTimeout derived from that — so a hung endpoint
// costs one reconcile ten seconds, not the reconcile worker forever, and a
// cancelled reconcile drops the request immediately.
func (e *ClusterVisionEnricher) Lookup(ctx context.Context, cves []string) (Response, error) {
	if len(cves) == 0 {
		// Never posted: an empty list earns a 400, which the ladder above would
		// read as a terminal failure of a perfectly clean image.
		return Response{}, errors.New("cve enrichment: refusing to look up an empty CVE set")
	}

	ids := append([]string(nil), cves...)
	sort.Strings(ids)
	key := memoKey(ids)

	if resp, ok := e.memoGet(key); ok {
		return resp, nil
	}

	budgetCtx, cancel := context.WithTimeout(ctx, TotalBudget)
	defer cancel()

	var agg Response
	first := true
	for start := 0; start < len(ids); start += maxChunk {
		end := min(start+maxChunk, len(ids))
		chunk, err := e.post(budgetCtx, ids[start:end])
		if err != nil {
			return Response{}, err
		}
		agg = mergeResponses(agg, chunk, first)
		first = false
	}

	e.memoPut(key, agg)
	return agg, nil
}

func (e *ClusterVisionEnricher) post(ctx context.Context, ids []string) (Response, error) {
	body, err := json.Marshal(request{CVEs: ids})
	if err != nil {
		return Response{}, fmt.Errorf("cve enrichment: encode request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, perRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("cve enrichment: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("cve enrichment: post %d id(s): %w", len(ids), err)
	}
	defer resp.Body.Close() //nolint:errcheck // close on a read-only body

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Response{}, fmt.Errorf("cve enrichment: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Response{}, &StatusError{Code: resp.StatusCode, Body: truncate(string(raw), 256)}
	}

	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return Response{}, fmt.Errorf("cve enrichment: decode response: %w", err)
	}
	return out, nil
}

// mergeResponses folds one chunk's answer into the aggregate.
//
// Every field is merged towards the LEAST confident reading, because a verdict
// built from several chunks can only be as trustworthy as its worst chunk:
// stale ORs, the cache totals take the minimum (so one chunk answered from an
// empty cache sinks the lot), and fetched_at keeps the oldest.
func mergeResponses(agg, chunk Response, first bool) Response {
	agg.Results = append(agg.Results, chunk.Results...)
	agg.Unknown = append(agg.Unknown, chunk.Unknown...)
	agg.Stale = agg.Stale || chunk.Stale

	if first {
		agg.KEVTotal = chunk.KEVTotal
		agg.EPSSTotal = chunk.EPSSTotal
		agg.FetchedAt = chunk.FetchedAt
		return agg
	}
	agg.KEVTotal = min(agg.KEVTotal, chunk.KEVTotal)
	agg.EPSSTotal = min(agg.EPSSTotal, chunk.EPSSTotal)
	if !chunk.FetchedAt.IsZero() && (agg.FetchedAt.IsZero() || chunk.FetchedAt.Before(agg.FetchedAt)) {
		agg.FetchedAt = chunk.FetchedAt
	}
	return agg
}

func (e *ClusterVisionEnricher) memoGet(key string) (Response, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry, ok := e.memo[key]
	if !ok || e.now().Sub(entry.at) > memoTTL {
		return Response{}, false
	}
	return entry.resp, true
}

func (e *ClusterVisionEnricher) memoPut(key string, resp Response) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	for k, v := range e.memo {
		if now.Sub(v.at) > memoTTL {
			delete(e.memo, k)
		}
	}
	if len(e.memo) >= memoMax {
		// Bounded, not clever: the table only ever holds one entry per in-flight
		// check, so overflowing it means something is very wrong and a clean
		// slate beats an eviction policy nobody will ever tune.
		e.memo = map[string]memoEntry{}
	}
	e.memo[key] = memoEntry{resp: resp, at: now}
}

func memoKey(sortedIDs []string) string {
	sum := sha256.Sum256([]byte(strings.Join(sortedIDs, ",")))
	return hex.EncodeToString(sum[:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
