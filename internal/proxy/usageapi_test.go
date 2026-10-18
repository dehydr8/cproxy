package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dehydr8/cproxy/internal/config"
	"github.com/dehydr8/cproxy/internal/pool"
)

// cannedUsageAPI is the wire shape of GET /api/oauth/usage (as seen by
// claude-swap): percentage utilization windows, optional spend and
// per-model weekly limits. Credit amounts are in cents; monthly_limit may be
// null; resets_at may be null.
const cannedUsageAPI = `{
  "five_hour":  {"utilization": 42.5, "resets_at": "2026-10-18T00:00:00Z"},
  "seven_day":  {"utilization": 12,   "resets_at": "2026-10-20T00:00:00Z"},
  "extra_usage": {"is_enabled": true, "used_credits": 2550, "monthly_limit": 50000,
                  "utilization": 5.1, "currency": "USD", "resets_at": null},
  "limits": [
    {"scope": {"model": {"display_name": "Fable"}}, "percent": 20,
     "resets_at": "2026-10-19T00:00:00Z"}
  ]
}`

// newStartedServer builds a Server (plus its httptest listener) whose
// poller is under test control: it returns the *Server so tests can drive
// pollUsageOnce deterministically instead of waiting for ticks.
func newStartedServer(t *testing.T, upstreamURL string, rt *config.Runtime) (*Server, *httptest.Server) {
	t.Helper()
	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Options{
		Upstream: u,
		State:    &State{Runtime: rt},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	t.Cleanup(srv.Close)
	return srv, ts
}

func usageAPIHits(t *testing.T, up *fakeUpstream) []hit {
	t.Helper()
	var out []hit
	for _, h := range up.recorded() {
		if h.path == usageAPIPath {
			out = append(out, h)
		}
	}
	return out
}

func TestFetchUsageAPI(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotAuth, gotBeta, gotUA string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath, gotAuth, gotBeta, gotUA = r.URL.Path,
			r.Header.Get("Authorization"), r.Header.Get("anthropic-beta"), r.Header.Get("User-Agent")
		mu.Unlock()
		io.WriteString(w, cannedUsageAPI)
	}))
	defer ts.Close()
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := fetchUsageAPI(ctx, http.DefaultClient, u, "sk-ant-oat01-tokA")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	mu.Lock()
	path, auth, beta, ua := gotPath, gotAuth, gotBeta, gotUA
	mu.Unlock()
	if path != usageAPIPath || auth != "Bearer sk-ant-oat01-tokA" || beta != "oauth-2025-04-20" || ua == "" {
		t.Fatalf("request shape: path=%q auth=%q beta=%q ua=%q", path, auth, beta, ua)
	}
	if got.FiveHour == nil || got.FiveHour.Pct != 42.5 || got.FiveHour.ResetsAt == nil {
		t.Fatalf("five_hour: %+v", got.FiveHour)
	}
	if got.SevenDay == nil || got.SevenDay.Pct != 12 {
		t.Fatalf("seven_day: %+v", got.SevenDay)
	}
	if got.Spend == nil || got.Spend.Used == nil || *got.Spend.Used != 25.5 ||
		got.Spend.Limit == nil || *got.Spend.Limit != 500 || got.Spend.Currency != "USD" {
		t.Fatalf("spend: %+v", got.Spend)
	}
	if len(got.Scoped) != 1 || got.Scoped[0].Name != "Fable" || got.Scoped[0].Pct != 20 {
		t.Fatalf("scoped: %+v", got.Scoped)
	}
	if got.FetchedAt.IsZero() {
		t.Fatalf("fetched_at: %v", got.FetchedAt)
	}
}

func TestUsageAPIPollerPreferredOverHeaders(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
		if h.path == usageAPIPath {
			io.WriteString(w, cannedUsageAPI)
			return
		}
		if h.path == profileAPIPath {
			io.WriteString(w, `{"account": {"uuid": "u1", "email": "main@example.com"}, "organization": {"name": "Acme", "organization_type": "claude_max", "rate_limit_tier": "default_claude_max_20x"}}`)
			return
		}
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Requests-Limit", "50")
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "42")
		w.WriteHeader(200)
	})
	accts := []pool.Account{
		{Name: "main", Type: pool.TokenTypeOAuth, Token: "sk-ant-oat01-tokMAIN"},
		{Name: "apikey", Type: pool.TokenTypeAPI, Token: "sk-ant-api03-tokKEY"},
	}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
	srv, ts := newStartedServer(t, up.URL, testRuntime(accts, clients))

	// One proxied request: main serves it (first in account order), so the
	// header-inferred fallback is recorded alongside the real usage API.
	if resp := doReq(t, "POST", ts.URL+"/v1/messages", bearerHeaders("cp_alice"), "{}"); resp.StatusCode != 200 {
		t.Fatalf("proxied request: %d", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.pollUsageOnce(ctx) // deterministic first cycle

	// Only the OAuth account is polled, with its own token.
	hits := usageAPIHits(t, up)
	if len(hits) != 1 || hits[0].auth != "Bearer sk-ant-oat01-tokMAIN" {
		t.Fatalf("usage API hits: %+v", hits)
	}

	// JSON carries the real usage, preferred, plus the header fallback.
	resp := doReq(t, "GET", ts.URL+"/usage?format=json", bearerHeaders("cp_alice"), "")
	var doc struct {
		Accounts []struct {
			Name     string `json:"name"`
			UsageAPI *struct {
				FiveHour struct {
					Pct float64 `json:"pct"`
				} `json:"five_hour"`
				SevenDay struct {
					Pct float64 `json:"pct"`
				} `json:"seven_day"`
				Spend struct {
					Used  *float64 `json:"used"`
					Limit *float64 `json:"limit"`
				} `json:"spend"`
				Scoped []struct {
					Name string  `json:"name"`
					Pct  float64 `json:"pct"`
				} `json:"scoped"`
			} `json:"usage_api"`
			UsageAPIErr string `json:"usage_api_error"`
			RateLimits  *struct {
				RequestsRemaining *int64 `json:"requests_remaining"`
			} `json:"rate_limits"`
		} `json:"accounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Accounts) != 2 {
		t.Fatalf("accounts: %d", len(doc.Accounts))
	}
	var main, apikey = doc.Accounts[0], doc.Accounts[1]
	if main.Name != "main" {
		main, apikey = apikey, main
	}
	if main.UsageAPI == nil || main.UsageAPI.FiveHour.Pct != 42.5 || main.UsageAPI.SevenDay.Pct != 12 ||
		main.UsageAPI.Spend.Used == nil || *main.UsageAPI.Spend.Used != 25.5 ||
		main.UsageAPI.Spend.Limit == nil || *main.UsageAPI.Spend.Limit != 500 ||
		len(main.UsageAPI.Scoped) != 1 || main.UsageAPI.Scoped[0].Name != "Fable" {
		t.Fatalf("main usage_api: %+v", main.UsageAPI)
	}
	if main.UsageAPIErr != "" {
		t.Fatalf("main usage_api_error: %q", main.UsageAPIErr)
	}
	if main.RateLimits == nil || main.RateLimits.RequestsRemaining == nil || *main.RateLimits.RequestsRemaining != 42 {
		t.Fatalf("main rate_limits fallback: %+v", main.RateLimits)
	}
	// The API-key account is not polled: no data, no error.
	if apikey.UsageAPI != nil || apikey.UsageAPIErr != "" {
		t.Fatalf("apikey usage_api: %+v err=%q", apikey.UsageAPI, apikey.UsageAPIErr)
	}

	// The dashboard renders the real numbers.
	frag := doReq(t, "GET", ts.URL+"/usage?fragment=1", bearerHeaders("cp_alice"), "")
	b, _ := io.ReadAll(frag.Body)
	for _, want := range []string{"42.5%", "12%", "$25.50 / $500", "Fable", "20%", "main@example.com", "Max 20x"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("fragment missing %q:\n%s", want, b)
		}
	}
	// Real numbers win: the header fallback is only shown without them.
	if strings.Contains(string(b), "from response headers") {
		t.Fatalf("header fallback shown next to usage API numbers:\n%s", b)
	}

	// Fresh entries are not re-fetched on the next cycle (60s TTL), and
	// the identity is fetched once.
	srv.pollUsageOnce(ctx)
	if got := len(usageAPIHits(t, up)); got != 1 {
		t.Fatalf("usage API re-fetched while fresh: %d hits", got)
	}
	profiles := 0
	for _, h := range up.recorded() {
		if h.path == profileAPIPath {
			profiles++
		}
	}
	if profiles != 1 {
		t.Fatalf("profile fetched %d times; want once, for the OAuth account only", profiles)
	}
}

// The first usage view must not wait for a tick: it starts the poller and
// its first cycle lands before the response is rendered.
func TestUsageAPILazilyStartedByFirstView(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
		if h.path == usageAPIPath {
			io.WriteString(w, cannedUsageAPI)
			return
		}
		w.WriteHeader(200)
	})
	accts := []pool.Account{{Name: "main", Type: pool.TokenTypeOAuth, Token: "sk-ant-oat01-tokMAIN"}}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
	srv, ts := newStartedServer(t, up.URL, testRuntime(accts, clients))
	_ = srv

	resp := doReq(t, "GET", ts.URL+"/usage?format=json", bearerHeaders("cp_alice"), "")
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"usage_api"`) {
		t.Fatalf("first view has no usage_api:\n%s", body)
	}
	if len(usageAPIHits(t, up)) != 1 {
		t.Fatalf("usage API hits after first view: %d", len(usageAPIHits(t, up)))
	}
}

func TestUsageAPIForbiddenBackoff(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
		if h.path == usageAPIPath {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(200)
	})
	accts := []pool.Account{{Name: "main", Type: pool.TokenTypeOAuth, Token: "sk-ant-oat01-tokMAIN"}}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
	srv, ts := newStartedServer(t, up.URL, testRuntime(accts, clients))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.pollUsageOnce(ctx)

	data, errText := srv.usage.snapshot("main")
	if data != nil || !strings.Contains(errText, "user:profile") {
		t.Fatalf("snapshot after 403: data=%v err=%q", data, errText)
	}

	// The dashboard surfaces the scope note instead of numbers.
	frag := doReq(t, "GET", ts.URL+"/usage?fragment=1", bearerHeaders("cp_alice"), "")
	b, _ := io.ReadAll(frag.Body)
	if !strings.Contains(string(b), "token needs the user:profile scope") {
		t.Fatalf("fragment missing the 403 note:\n%s", b)
	}

	// 403 backs off long: an immediate second cycle must not re-fetch.
	srv.pollUsageOnce(ctx)
	if got := len(usageAPIHits(t, up)); got != 1 {
		t.Fatalf("403 was retried immediately: %d hits", got)
	}
}

func TestUsageAPIRetryAfterHonored(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
		if h.path == usageAPIPath {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(200)
	})
	accts := []pool.Account{{Name: "main", Type: pool.TokenTypeOAuth, Token: "sk-ant-oat01-tokMAIN"}}
	srv, _ := newStartedServer(t, up.URL, testRuntime(accts, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.pollUsageOnce(ctx)

	srv.usage.mu.Lock()
	e := srv.usage.entries["main"]
	srv.usage.mu.Unlock()
	if e == nil || !strings.Contains(e.err, "429") {
		t.Fatalf("entry after 429: %+v", e)
	}
	if wait := time.Until(e.retryAt); wait < 100*time.Second {
		t.Fatalf("Retry-After 120 not honored: retry in %v", wait)
	}
	if e.data != nil {
		t.Fatalf("failed entry kept data: %+v", e.data)
	}
}

func TestUsageAPISummary(t *testing.T) {
	now := time.Date(2026, 10, 17, 22, 0, 0, 0, time.UTC)
	u := &UsageAPI{
		FiveHour: &UsageAPIWindow{Pct: 42.5, ResetsAt: ptrTime(now.Add(2 * time.Hour))},
		SevenDay: &UsageAPIWindow{Pct: 12},
	}
	if got := UsageAPISummary(u, "", now); got != "5h 42.5% (resets 2h) · 7d 12%" {
		t.Fatalf("summary: %q", got)
	}
	if got := UsageAPISummary(nil, "usage API: 403 (token needs the user:profile scope)", now); got != "usage API: 403 (token needs the user:profile scope)" {
		t.Fatalf("error summary: %q", got)
	}
	if got := UsageAPISummary(nil, "", now); got != "—" {
		t.Fatalf("empty summary: %q", got)
	}
	if got := UsageAPISummary(&UsageAPI{}, "", now); got != "no usage reported" {
		t.Fatalf("bare summary: %q", got)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
