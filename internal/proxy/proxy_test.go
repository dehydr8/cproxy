package proxy

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dehydr8/cproxy/internal/config"
	"github.com/dehydr8/cproxy/internal/pool"
)

// hit records one request seen by the fake upstream.
type hit struct {
	auth   string
	apiKey string
	method string
	path   string
	body   string
}

type fakeUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	hits []hit
}

func newFakeUpstream(t *testing.T, handle func(w http.ResponseWriter, h hit)) *fakeUpstream {
	t.Helper()
	up := &fakeUpstream{}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h := hit{
			auth:   r.Header.Get("Authorization"),
			apiKey: r.Header.Get("X-Api-Key"),
			method: r.Method,
			path:   r.URL.Path,
			body:   string(b),
		}
		up.mu.Lock()
		up.hits = append(up.hits, h)
		up.mu.Unlock()
		handle(w, h)
	}))
	t.Cleanup(up.Close)
	return up
}

func (up *fakeUpstream) recorded() []hit {
	up.mu.Lock()
	defer up.mu.Unlock()
	out := make([]hit, len(up.hits))
	copy(out, up.hits)
	return out
}

func (up *fakeUpstream) auths() []string {
	var out []string
	for _, h := range up.recorded() {
		out = append(out, h.auth+h.apiKey)
	}
	return out
}

// newProxy starts a cproxy handler fronting upstreamURL with the given state.
func newProxy(t *testing.T, upstreamURL string, rt *config.Runtime, sessionToken string, tweak func(*Options)) *httptest.Server {
	t.Helper()
	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Upstream: u,
		State:    &State{Runtime: rt, SessionToken: sessionToken},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if tweak != nil {
		tweak(&opts)
	}
	ts := httptest.NewServer(New(opts))
	t.Cleanup(ts.Close)
	return ts
}

func testRuntime(accts []pool.Account, clients map[string]config.ClientAuth) *config.Runtime {
	if clients == nil {
		clients = map[string]config.ClientAuth{}
	}
	return &config.Runtime{
		Accounts:    accts,
		ClientAuth:  clients,
		DefaultPool: config.DefaultPoolName,
	}
}

func doReq(t *testing.T, method, base string, headers map[string]string, body string) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, base, rd)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func errType(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	var e anthropicError
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("response is not an Anthropic-style error: %s", b)
	}
	return e.Error.Type
}

func aliceHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer cp_alice", "X-Api-Key": "leak-attempt", "anthropic-version": "2023-06-01"}
}

func TestClientAuth(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) { w.WriteHeader(200) })
	accts := []pool.Account{{Name: "a", Token: "sk-ant-oat01-a"}}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
	ts := newProxy(t, up.URL, testRuntime(accts, clients), "", nil)

	resp := doReq(t, "POST", ts.URL+"/v1/messages", nil, "{}")
	if resp.StatusCode != 401 || errType(t, resp) != "authentication_error" {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	resp = doReq(t, "POST", ts.URL+"/v1/messages", map[string]string{"Authorization": "Bearer cp_wrong"}, "{}")
	if resp.StatusCode != 401 {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	resp = doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), "{}")
	if resp.StatusCode != 200 {
		t.Fatalf("right token: %d", resp.StatusCode)
	}
}

func TestSessionTokenAuth(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) { w.WriteHeader(200) })
	accts := []pool.Account{{Name: "a", Token: "sk-ant-oat01-a"}}
	ts := newProxy(t, up.URL, testRuntime(accts, nil), "cp_local", nil)

	resp := doReq(t, "POST", ts.URL+"/v1/messages", map[string]string{"Authorization": "Bearer cp_local"}, "{}")
	if resp.StatusCode != 200 {
		t.Fatalf("session token rejected: %d", resp.StatusCode)
	}
	resp = doReq(t, "POST", ts.URL+"/v1/messages", map[string]string{"Authorization": "Bearer cp_other"}, "{}")
	if resp.StatusCode != 401 {
		t.Fatalf("non-session token accepted: %d", resp.StatusCode)
	}
}

func TestRewritesCredentials(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) { w.WriteHeader(200) })
	accts := []pool.Account{
		{Name: "oauth", Token: "sk-ant-oat01-secret", Type: pool.TokenTypeOAuth},
		{Name: "api", Token: "sk-ant-api03-secret", Type: pool.TokenTypeAPI},
	}
	clients := map[string]config.ClientAuth{
		"cp_alice": {Name: "alice"},
		"cp_bob":   {Name: "bob"},
	}
	ts := newProxy(t, up.URL, testRuntime(accts, clients), "", nil)

	doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), `{"m":1}`)
	doReq(t, "POST", ts.URL+"/v1/messages",
		map[string]string{"Authorization": "Bearer cp_bob", "X-Api-Key": "leak-attempt"}, `{"m":2}`)

	hits := up.recorded()
	if len(hits) != 2 {
		t.Fatalf("upstream hits = %d", len(hits))
	}
	if hits[0].auth != "Bearer sk-ant-oat01-secret" || hits[0].apiKey != "" {
		t.Fatalf("oauth rewrite: auth=%q apiKey=%q", hits[0].auth, hits[0].apiKey)
	}
	if hits[1].auth != "" || hits[1].apiKey != "sk-ant-api03-secret" {
		t.Fatalf("api rewrite: auth=%q apiKey=%q", hits[1].auth, hits[1].apiKey)
	}
	for i, h := range hits {
		if strings.Contains(h.auth, "cp_") || strings.Contains(h.apiKey, "leak") {
			t.Fatalf("client credential leaked upstream (hit %d): %+v", i, h)
		}
		if h.path != "/v1/messages" {
			t.Fatalf("path rewritten: %q", h.path)
		}
	}
}

func TestFailoverOn429WithSticky(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
		if h.auth == "Bearer tokA" {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(429)
			w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"a is limited"}}`))
			return
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	})
	// Three accounts: after failing over to b, a fresh remap would rotate
	// alice on to c, paying her context a third time.
	accts := []pool.Account{
		{Name: "a", Token: "tokA"},
		{Name: "b", Token: "tokB"},
		{Name: "c", Token: "tokC"},
	}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
	ts := newProxy(t, up.URL, testRuntime(accts, clients), "", nil)

	body := `{"messages":[{"role":"user","content":"hi"}]}`
	resp := doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), body)
	if resp.StatusCode != 200 {
		t.Fatalf("request 1: %d (wanted failover to succeed)", resp.StatusCode)
	}
	hits := up.recorded()
	if len(hits) != 2 || hits[0].auth != "Bearer tokA" || hits[1].auth != "Bearer tokB" {
		t.Fatalf("attempt 1 hits = %s", up.auths())
	}
	if hits[0].body != body || hits[1].body != body {
		t.Fatalf("retried body differs: %q vs %q", hits[0].body, hits[1].body)
	}

	// Second request must go straight to b: a is cooling down and the
	// client stays pinned to its failover target.
	resp = doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), body)
	if resp.StatusCode != 200 {
		t.Fatalf("request 2: %d", resp.StatusCode)
	}
	hits = up.recorded()
	if len(hits) != 3 || hits[2].auth != "Bearer tokB" {
		t.Fatalf("request 2 hit %s, want only tokB (a cooling)", up.auths())
	}
}

func TestFailoverOn401MarksInvalid(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
		if h.auth == "Bearer tokA" {
			w.WriteHeader(401)
			w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"bad token"}}`))
			return
		}
		w.WriteHeader(200)
	})
	accts := []pool.Account{{Name: "a", Token: "tokA"}, {Name: "b", Token: "tokB"}}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
	ts := newProxy(t, up.URL, testRuntime(accts, clients), "", nil)

	resp := doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), "{}")
	if resp.StatusCode != 200 {
		t.Fatalf("request 1: %d", resp.StatusCode)
	}
	if got := up.auths(); len(got) != 2 || got[0] != "Bearer tokA" || got[1] != "Bearer tokB" {
		t.Fatalf("attempt 1 = %s", got)
	}
	// a is invalid: every later request uses b only.
	doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), "{}")
	doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), "{}")
	if got := up.auths(); len(got) != 4 {
		t.Fatalf("after invalid: %s", got)
	}
}

func TestAllAccounts429(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(429)
		w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"everyone is limited"}}`))
	})
	accts := []pool.Account{{Name: "a", Token: "tokA"}}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
	ts := newProxy(t, up.URL, testRuntime(accts, clients), "", nil)

	resp := doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), "{}")
	if resp.StatusCode != 429 {
		t.Fatalf("got %d, want 429", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "everyone is limited") {
		t.Fatalf("upstream 429 body not surfaced: %s", b)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "9" {
		t.Fatalf("Retry-After = %q", ra)
	}

	// While the only account cools down, requests get a synthesized 429
	// with a matching Retry-After.
	resp = doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), "{}")
	if resp.StatusCode != 429 || errType(t, resp) != "rate_limit_error" {
		t.Fatalf("cooling response: %d", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("cooling Retry-After = %q", ra)
	}
}

func TestRateLimitCooldown(t *testing.T) {
	reset := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	epoch := strconv.FormatInt(reset.Unix(), 10)
	for _, tc := range []struct {
		name    string
		headers map[string]string
		min     time.Duration // cooldown bounds, from now
		max     time.Duration
		exactly *time.Time
	}{
		{
			// Subscription exhausted: no Retry-After, only the unified reset.
			name:    "exhausted subscription waits for its reset",
			headers: map[string]string{"Anthropic-Ratelimit-Unified-Status": "rejected", "Anthropic-Ratelimit-Unified-Reset": epoch},
			exactly: &reset,
		},
		{
			// The reset is sent on every response; a 429 that is not the
			// subscription running out must not bench the account until it.
			name:    "other 429 with a reset header pauses briefly",
			headers: map[string]string{"Anthropic-Ratelimit-Unified-Status": "allowed", "Anthropic-Ratelimit-Unified-Reset": epoch},
			min:     briefCooldown - 2*time.Second, max: briefCooldown + 2*time.Second,
		},
		{
			name:    "other 429 honors Retry-After",
			headers: map[string]string{"Retry-After": "90"},
			min:     88 * time.Second, max: 92 * time.Second,
		},
		{
			name:    "exhausted without a reset falls back",
			headers: map[string]string{"Anthropic-Ratelimit-Unified-Status": "rejected"},
			min:     exhaustedCooldown - 2*time.Second, max: exhaustedCooldown + 2*time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(429)
			})
			sel := pool.NewSelector()
			accts := []pool.Account{{Name: "a", Token: "tokA"}}
			clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
			ts := newProxy(t, up.URL, testRuntime(accts, clients), "", func(o *Options) { o.Selector = sel })

			if resp := doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), "{}"); resp.StatusCode != 429 {
				t.Fatalf("got %d, want 429", resp.StatusCode)
			}
			got := sel.StatsSnapshot([]string{"a"})["a"].CoolingUntil
			if tc.exactly != nil {
				if !got.Equal(*tc.exactly) {
					t.Fatalf("cooling until %v, want %v", got, *tc.exactly)
				}
				return
			}
			if d := time.Until(got); d < tc.min || d > tc.max {
				t.Fatalf("cooldown %v, want between %v and %v", d.Round(time.Second), tc.min, tc.max)
			}
		})
	}
}

func TestBare429DoesNotBenchPool(t *testing.T) {
	// The real shape of an OAuth refusal (here: a request without Claude
	// Code's system prompt): 429, no rate-limit headers, no Retry-After.
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
		if h.body == "bare" {
			w.Header().Set("X-Should-Retry", "true")
			w.WriteHeader(429)
			io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`)
			return
		}
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "allowed")
		w.WriteHeader(200)
	})
	sel := pool.NewSelector()
	accts := []pool.Account{{Name: "a", Token: "tokA"}, {Name: "b", Token: "tokB"}}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
	ts := newProxy(t, up.URL, testRuntime(accts, clients), "", func(o *Options) { o.Selector = sel })

	resp := doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), "bare")
	if resp.StatusCode != 429 {
		t.Fatalf("bare request: %d, want the upstream 429 passed through", resp.StatusCode)
	}
	if b, _ := io.ReadAll(resp.Body); !strings.Contains(string(b), `"message":"Error"`) {
		t.Fatalf("client got %s, want the upstream body", b)
	}
	for name, st := range sel.StatsSnapshot([]string{"a", "b"}) {
		if !st.CoolingUntil.IsZero() || st.RateLimited != 0 {
			t.Fatalf("account %s benched by a bare 429: %+v", name, st)
		}
	}
	// The pool is intact: the next normal request is served at once.
	if resp := doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), "{}"); resp.StatusCode != 200 {
		t.Fatalf("normal request after a bare 429: %d (%s)", resp.StatusCode, errType(t, resp))
	}
}

func TestClientCancelDoesNotBenchPool(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
		if h.body == "slow" {
			started <- struct{}{}
			<-release
			return
		}
		w.WriteHeader(200)
	})
	t.Cleanup(func() { close(release) }) // runs before up.Close
	accts := []pool.Account{{Name: "a", Token: "tokA"}, {Name: "b", Token: "tokB"}}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
	ts := newProxy(t, up.URL, testRuntime(accts, clients), "", nil)

	// The client gives up while the upstream is still thinking (Esc in Claude).
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", ts.URL+"/v1/messages", strings.NewReader("slow"))
	req.Header.Set("Authorization", "Bearer cp_alice")
	errc := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		errc <- err
	}()
	<-started
	cancel()
	<-errc

	// Neither account was at fault: the next request must be served, by
	// the account the client was already on.
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp := doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), "{}")
		if resp.StatusCode == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after a client cancel: %d (%s), want 200", resp.StatusCode, errType(t, resp))
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := up.auths()
	if got[len(got)-1] != "Bearer tokA" {
		t.Fatalf("hits after cancel = %s; want the client kept on tokA", got)
	}
	for _, a := range got[1 : len(got)-1] {
		if a != "Bearer tokA" {
			t.Fatalf("canceled request was failed over: %s", got)
		}
	}
}

func TestStreamingPassthrough(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		for _, ev := range []string{"event: 1", "event: 2", "event: 3"} {
			fmt.Fprintf(w, "data: %s\n\n", ev)
			fl.Flush()
		}
	})
	accts := []pool.Account{{Name: "a", Token: "tokA"}}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
	ts := newProxy(t, up.URL, testRuntime(accts, clients), "", nil)

	resp := doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), "{}")
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	var got []string
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data: ") {
			got = append(got, strings.TrimPrefix(line, "data: "))
		}
	}
	if strings.Join(got, ",") != "event: 1,event: 2,event: 3" {
		t.Fatalf("stream events = %v", got)
	}
}

func TestBodyOverLimitSkipsFailover(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
		if h.auth == "Bearer tokA" {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(200)
	})
	accts := []pool.Account{{Name: "a", Token: "tokA"}, {Name: "b", Token: "tokB"}}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
	ts := newProxy(t, up.URL, testRuntime(accts, clients), "", func(o *Options) {
		o.MaxBody = 8
	})

	big := strings.Repeat("x", 20)
	resp := doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), big)
	if resp.StatusCode != 429 {
		t.Fatalf("got %d, want passthrough 429", resp.StatusCode)
	}
	if got := up.auths(); len(got) != 1 || got[0] != "Bearer tokA" {
		t.Fatalf("over-limit body was retried: %s", got)
	}
	// The oversized body must still be forwarded intact.
	if hits := up.recorded(); hits[0].body != big {
		t.Fatalf("oversized body corrupted: %q", hits[0].body)
	}
}

func TestClientPoolOverride(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
		w.WriteHeader(200)
		w.Write([]byte(`{"who":"` + h.auth + `"}`))
	})
	accts := []pool.Account{
		{Name: "default-a", Token: "tokDefault"},
		{Name: "cvp-a", Token: "tokCvp", Pools: []string{"cvp"}},
	}
	clients := map[string]config.ClientAuth{
		"cp_alice": {Name: "alice", Pool: "cvp"},
		"cp_bob":   {Name: "bob"},
	}
	ts := newProxy(t, up.URL, testRuntime(accts, clients), "", nil)

	doReq(t, "POST", ts.URL+"/v1/messages", map[string]string{"Authorization": "Bearer cp_alice"}, "{}")
	doReq(t, "POST", ts.URL+"/v1/messages", map[string]string{"Authorization": "Bearer cp_bob"}, "{}")

	hits := up.recorded()
	if len(hits) != 2 {
		t.Fatalf("hits = %d", len(hits))
	}
	if hits[0].auth != "Bearer tokCvp" {
		t.Fatalf("alice (cvp) used %q", hits[0].auth)
	}
	if hits[1].auth != "Bearer tokDefault" {
		t.Fatalf("bob (default) used %q", hits[1].auth)
	}
}

func TestUnknownClientPool(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) { w.WriteHeader(200) })
	accts := []pool.Account{{Name: "a", Token: "tokA"}}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice", Pool: "nope"}}
	ts := newProxy(t, up.URL, testRuntime(accts, clients), "", nil)

	resp := doReq(t, "POST", ts.URL+"/v1/messages", map[string]string{"Authorization": "Bearer cp_alice"}, "{}")
	if resp.StatusCode != 503 || errType(t, resp) != "api_error" {
		t.Fatalf("unknown client pool: %d", resp.StatusCode)
	}
}

func TestGetRequestsProxied(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
		w.WriteHeader(200)
		w.Write([]byte(`{"models":[]}`))
	})
	accts := []pool.Account{{Name: "a", Token: "tokA"}}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
	ts := newProxy(t, up.URL, testRuntime(accts, clients), "", nil)

	resp := doReq(t, "GET", ts.URL+"/v1/models?q=1", aliceHeaders(), "")
	if resp.StatusCode != 200 {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
	hits := up.recorded()
	if len(hits) != 1 || hits[0].method != "GET" || hits[0].path != "/v1/models" || hits[0].body != "" {
		t.Fatalf("GET hit = %+v", hits[0])
	}
}

func TestUsageDashboard(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
		if h.auth == "Bearer tokA" {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(429)
			w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limited"}}`))
			return
		}
		w.WriteHeader(200)
	})
	accts := []pool.Account{{Name: "a", Token: "tokA"}, {Name: "b", Token: "tokB"}}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
	ts := newProxy(t, up.URL, testRuntime(accts, clients), "", nil)

	// Request 1: a 429s, fails over to b. Request 2: sticky, still b.
	for range 2 {
		if resp := doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), "{}"); resp.StatusCode != 200 {
			t.Fatalf("proxied request: %d", resp.StatusCode)
		}
	}

	page := doReq(t, "GET", ts.URL+"/usage?token=cp_alice", nil, "")
	if page.StatusCode != 200 {
		t.Fatalf("usage page: %d", page.StatusCode)
	}
	if ct := page.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("usage content type: %q", ct)
	}
	body, _ := io.ReadAll(page.Body)
	for _, want := range []string{
		"setInterval",      // live-refresh wiring (inline, no third-party script)
		`data-account="a"`, // account rows
		`data-account="b"`,
		"cooling",             // a is cooling after its 429
		`data-client="alice"`, // client row
		"token=cp_alice",      // ?token= propagated into the fragment URL
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("usage page missing %q:\n%s", want, body)
		}
	}

	if strings.Contains(string(body), "<script src") {
		t.Fatalf("usage page loads an external script while carrying ?token=:\n%s", body)
	}

	frag := doReq(t, "GET", ts.URL+"/usage?fragment=1&token=cp_alice", nil, "")
	if frag.StatusCode != 200 {
		t.Fatalf("fragment: %d", frag.StatusCode)
	}
	fb, _ := io.ReadAll(frag.Body)
	if strings.Contains(string(fb), "<html") || !strings.Contains(string(fb), `data-account="b"`) {
		t.Fatalf("fragment is not a table-only partial:\n%s", fb)
	}

	if resp := doReq(t, "GET", ts.URL+"/usage", nil, ""); resp.StatusCode != 401 {
		t.Fatalf("unauthenticated usage: %d", resp.StatusCode)
	}
	if resp := doReq(t, "GET", ts.URL+"/usage?token=cp_wrong", nil, ""); resp.StatusCode != 401 {
		t.Fatalf("bad query token: %d", resp.StatusCode)
	}
}

func bearerHeaders(tok string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + tok}
}

func TestUsageScopes(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) { w.WriteHeader(200) })
	accts := []pool.Account{{Name: "a", Token: "tokA"}}
	clients := map[string]config.ClientAuth{
		"cp_full":    {Name: "full"},
		"cp_monitor": {Name: "monitor", Scopes: []string{config.ScopeUsage}},
		"cp_worker":  {Name: "worker", Scopes: []string{config.ScopeProxy}},
	}
	ts := newProxy(t, up.URL, testRuntime(accts, clients), "", nil)

	// A token with no scopes has full access.
	if resp := doReq(t, "POST", ts.URL+"/v1/messages", bearerHeaders("cp_full"), "{}"); resp.StatusCode != 200 {
		t.Fatalf("full proxy: %d", resp.StatusCode)
	}
	if resp := doReq(t, "GET", ts.URL+"/usage", bearerHeaders("cp_full"), ""); resp.StatusCode != 200 {
		t.Fatalf("full usage: %d", resp.StatusCode)
	}

	// usage-only: dashboard yes, proxying no.
	if resp := doReq(t, "GET", ts.URL+"/usage", bearerHeaders("cp_monitor"), ""); resp.StatusCode != 200 {
		t.Fatalf("monitor usage: %d", resp.StatusCode)
	}
	resp := doReq(t, "POST", ts.URL+"/v1/messages", bearerHeaders("cp_monitor"), "{}")
	if resp.StatusCode != 403 || errType(t, resp) != "permission_error" {
		t.Fatalf("monitor proxy: %d", resp.StatusCode)
	}

	// proxy-only: proxying yes, dashboard no.
	if resp := doReq(t, "POST", ts.URL+"/v1/messages", bearerHeaders("cp_worker"), "{}"); resp.StatusCode != 200 {
		t.Fatalf("worker proxy: %d", resp.StatusCode)
	}
	resp = doReq(t, "GET", ts.URL+"/usage", bearerHeaders("cp_worker"), "")
	if resp.StatusCode != 403 || errType(t, resp) != "permission_error" {
		t.Fatalf("worker usage: %d", resp.StatusCode)
	}
}

func TestUsageJSONAndRateLimits(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Requests-Limit", "50")
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "42")
		w.Header().Set("Anthropic-Ratelimit-Requests-Reset", time.Now().Add(5*time.Minute).UTC().Format(time.RFC3339))
		w.Header().Set("Anthropic-Ratelimit-Tokens-Limit", "40000")
		w.Header().Set("Anthropic-Ratelimit-Tokens-Remaining", "38000")
		w.Header().Set("Anthropic-Ratelimit-Tokens-Reset", "1920") // delta seconds
		// Subscription windows, as real OAuth responses send them.
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.11")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", "1792298400")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.07")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Reset", "1792792800")
		w.Header().Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")
		w.Header().Set("Anthropic-Ratelimit-Unified-Overage-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-Overage-Disabled-Reason", "org_spend_cap_reached")
		w.WriteHeader(200)
	})
	accts := []pool.Account{{Name: "a", Token: "tokA"}}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
	ts := newProxy(t, up.URL, testRuntime(accts, clients), "", nil)

	if resp := doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), "{}"); resp.StatusCode != 200 {
		t.Fatalf("proxy: %d", resp.StatusCode)
	}

	resp := doReq(t, "GET", ts.URL+"/usage?format=json", aliceHeaders(), "")
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("json usage: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var doc struct {
		UptimeSeconds float64 `json:"uptime_seconds"`
		Accounts      []struct {
			Name       string `json:"name"`
			Requests   int64  `json:"requests"`
			RateLimits *struct {
				Status            string     `json:"status"`
				RequestsRemaining *int64     `json:"requests_remaining"`
				TokensRemaining   *int64     `json:"tokens_remaining"`
				FiveHourPct       *float64   `json:"five_hour_pct"`
				FiveHourReset     *time.Time `json:"five_hour_reset"`
				SevenDayPct       *float64   `json:"seven_day_pct"`
				Claim             string     `json:"claim"`
				OverageReason     string     `json:"overage_reason"`
			} `json:"rate_limits"`
		} `json:"accounts"`
		Clients []struct {
			Name     string `json:"name"`
			Pinned   string `json:"pinned"`
			Requests int64  `json:"requests"`
		} `json:"clients"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.UptimeSeconds <= 0 || len(doc.Accounts) != 1 || len(doc.Clients) != 1 {
		t.Fatalf("json shape: %+v", doc)
	}
	acc := doc.Accounts[0]
	if acc.Name != "a" || acc.Requests != 1 {
		t.Fatalf("account: %+v", acc)
	}
	if acc.RateLimits == nil || acc.RateLimits.Status != "allowed" ||
		acc.RateLimits.RequestsRemaining == nil || *acc.RateLimits.RequestsRemaining != 42 ||
		acc.RateLimits.TokensRemaining == nil || *acc.RateLimits.TokensRemaining != 38000 {
		t.Fatalf("rate limits: %+v", acc.RateLimits)
	}
	if rl := acc.RateLimits; rl.FiveHourPct == nil || *rl.FiveHourPct != 11 || rl.FiveHourReset == nil ||
		rl.FiveHourReset.Unix() != 1792298400 || rl.SevenDayPct == nil || *rl.SevenDayPct != 7 ||
		rl.Claim != "five_hour" || rl.OverageReason != "org_spend_cap_reached" {
		t.Fatalf("subscription windows: %+v", rl)
	}
	cli := doc.Clients[0]
	if cli.Name != "alice" || cli.Pinned != "a" || cli.Requests != 1 {
		t.Fatalf("client: %+v", cli)
	}

	frag := doReq(t, "GET", ts.URL+"/usage?fragment=1", aliceHeaders(), "")
	b, _ := io.ReadAll(frag.Body)
	// Subscription windows from the headers become meters on the dashboard.
	for _, want := range []string{"5-hour", "11%", "7-day", "7%", "from response headers as of the last response"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("fragment missing %q:\n%s", want, b)
		}
	}
}

func TestHideUsageSplitsListeners(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) { w.WriteHeader(200) })
	u, _ := url.Parse(up.URL)
	accts := []pool.Account{{Name: "a", Token: "tokA"}}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
	srv := New(Options{Upstream: u, State: &State{Runtime: testRuntime(accts, clients)},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), HideUsage: true})
	t.Cleanup(srv.Close)
	public := httptest.NewServer(srv)
	t.Cleanup(public.Close)
	private := httptest.NewServer(srv.UsageHandler())
	t.Cleanup(private.Close)

	// Public: the API works; the dashboard and /_cproxy/* do not exist,
	// even for a valid token.
	if resp := doReq(t, "POST", public.URL+"/v1/messages", aliceHeaders(), "{}"); resp.StatusCode != 200 {
		t.Fatalf("public API: %d", resp.StatusCode)
	}
	for _, p := range []string{"/usage", "/usage?format=json", WhoamiPath} {
		if resp := doReq(t, "GET", public.URL+p, aliceHeaders(), ""); resp.StatusCode != 404 {
			t.Fatalf("public %s: %d, want 404", p, resp.StatusCode)
		}
	}

	// Private: the dashboard (still authenticated) and nothing else.
	if resp := doReq(t, "GET", private.URL+"/usage?format=json", aliceHeaders(), ""); resp.StatusCode != 200 {
		t.Fatalf("private usage: %d", resp.StatusCode)
	}
	if resp := doReq(t, "GET", private.URL+"/usage", nil, ""); resp.StatusCode != 401 {
		t.Fatalf("private usage without a token: %d, want 401", resp.StatusCode)
	}
	if resp := doReq(t, "POST", private.URL+"/v1/messages", aliceHeaders(), "{}"); resp.StatusCode != 404 {
		t.Fatalf("private API: %d, want 404", resp.StatusCode)
	}
	if got := len(up.recorded()); got != 1 {
		t.Fatalf("upstream saw %d requests; want only the public API call", got)
	}
}

func TestLiveRequestsAndTokens(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
		switch h.body {
		case "stream":
			// A streaming reply held open mid-way, as a long answer would be.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":"+
				"{\"input_tokens\":10,\"cache_creation_input_tokens\":5,\"cache_read_input_tokens\":1000,\"output_tokens\":1}}}\n\n")
			w.(http.Flusher).Flush()
			started <- struct{}{}
			<-release
			io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}\n\n")
		case "gzip":
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(200)
			zw := gzip.NewWriter(w)
			io.WriteString(zw, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"output_tokens\":1}}}\n\n"+
				"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n")
			zw.Close()
		default: // plain JSON
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"type":"message","usage":{"input_tokens":3,"output_tokens":4}}`)
		}
	})
	accts := []pool.Account{{Name: "a", Token: "tokA"}}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
	ts := newProxy(t, up.URL, testRuntime(accts, clients), "", nil)

	snapshot := func() (UsageAccount, UsageClient) {
		t.Helper()
		resp := doReq(t, "GET", ts.URL+"/usage?format=json", aliceHeaders(), "")
		var snap UsageSnapshot
		if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
			t.Fatal(err)
		}
		return snap.Accounts[0], snap.Clients[0]
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.DefaultClient.Do(func() *http.Request {
			r, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader("stream"))
			r.Header.Set("Authorization", "Bearer cp_alice")
			return r
		}())
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	<-started

	// Mid-stream: the account and the client show the open request, and
	// the account lists the client pinned to it.
	acc, cli := snapshot()
	if acc.InFlight != 1 || cli.Live != 1 || !slices.Equal(acc.PinnedBy, []string{"alice"}) {
		t.Fatalf("mid-stream: account in_flight=%d pinned_by=%v, client live=%d; want 1, [alice], 1", acc.InFlight, acc.PinnedBy, cli.Live)
	}
	frag := doReq(t, "GET", ts.URL+"/usage?fragment=1", aliceHeaders(), "")
	if b, _ := io.ReadAll(frag.Body); !strings.Contains(string(b), "1 streaming") {
		t.Fatal("dashboard does not show the open stream")
	}

	close(release)
	<-done
	for _, body := range []string{"gzip", "{}"} {
		io.ReadAll(doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), body).Body)
	}

	// After: nothing open; tokens from all three replies are counted, with
	// the stream's final (cumulative) output count, not its first. A client
	// can read the last byte just before the proxy's handler returns, so
	// give the counters a moment to settle.
	want := pool.TokenUsage{Input: 10 + 100 + 3, Output: 42 + 7 + 4, CacheRead: 1000, CacheWrite: 5}
	deadline := time.Now().Add(2 * time.Second)
	for {
		acc, cli = snapshot()
		if acc.InFlight == 0 && cli.Live == 0 && acc.Tokens == want || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if acc.InFlight != 0 || cli.Live != 0 {
		t.Fatalf("after: in_flight=%d live=%d, want 0", acc.InFlight, cli.Live)
	}
	if acc.Tokens != want || cli.Tokens != want {
		t.Fatalf("tokens: account %+v, client %+v; want %+v", acc.Tokens, cli.Tokens, want)
	}
}

func TestStickyMovesOffANearlySpentAccount(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
		// Real OAuth responses report the subscription windows (0–1).
		util := map[string]string{"Bearer tokA": "0.95", "Bearer tokB": "0.10"}[h.auth]
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", util)
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.05")
		w.WriteHeader(200)
	})
	accts := []pool.Account{{Name: "a", Token: "tokA"}, {Name: "b", Token: "tokB"}}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
	ts := newProxy(t, up.URL, testRuntime(accts, clients), "", nil)

	for range 3 {
		if resp := doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), "{}"); resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	// a serves first and reports 95% of its 5-hour window; from then on
	// alice is on b, before a ever returns a 429.
	if got := up.auths(); !slices.Equal(got, []string{"Bearer tokA", "Bearer tokB", "Bearer tokB"}) {
		t.Fatalf("served by %v; want a once, then b", got)
	}
}

func TestHelloProbeForwardedWithoutCredentials(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
		w.WriteHeader(200)
		io.WriteString(w, "hello")
	})
	accts := []pool.Account{{Name: "a", Token: "tokA"}}
	ts := newProxy(t, up.URL, testRuntime(accts, nil), "", nil)

	// Claude Code probes connectivity with no token: answered from the
	// upstream, and no account credential is attached to the probe.
	resp := doReq(t, "HEAD", ts.URL+"/api/hello", nil, "")
	if resp.StatusCode != 200 {
		t.Fatalf("probe: %d", resp.StatusCode)
	}
	hits := up.recorded()
	if len(hits) != 1 || hits[0].path != "/api/hello" || hits[0].auth != "" || hits[0].apiKey != "" {
		t.Fatalf("upstream saw %+v; want one credential-less /api/hello", hits)
	}
	// Everything else still needs a client token.
	if resp := doReq(t, "POST", ts.URL+"/v1/messages", nil, "{}"); resp.StatusCode != 401 {
		t.Fatalf("unauthenticated API call: %d", resp.StatusCode)
	}
}
