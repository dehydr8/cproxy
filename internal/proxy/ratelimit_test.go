package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dehydr8/cproxy/internal/config"
	"github.com/dehydr8/cproxy/internal/pool"
)

func TestClassify429(t *testing.T) {
	now := time.Now()
	in := func(d time.Duration) string { return strconv.FormatInt(now.Add(d).Unix(), 10) }
	for _, tc := range []struct {
		name    string
		headers map[string]string
		body    string
		want    limitScope
		min     time.Duration // cooldown bounds from now, for limits
		max     time.Duration
	}{
		{
			name: "spent 5-hour window limits the account",
			headers: map[string]string{
				"Anthropic-Ratelimit-Unified-Status":    "rejected",
				"Anthropic-Ratelimit-Unified-5h-Status": "rejected",
				"Anthropic-Ratelimit-Unified-5h-Reset":  in(2 * time.Hour),
				"Anthropic-Ratelimit-Unified-7d-Status": "allowed",
			},
			want: limitAccount, min: 2*time.Hour - time.Second, max: 2*time.Hour + resetJitter + time.Second,
		},
		{
			// A per-model weekly limit: the shared windows still have room.
			name: "per-model weekly limit limits the model",
			headers: map[string]string{
				"Anthropic-Ratelimit-Unified-Status":       "rejected",
				"Anthropic-Ratelimit-Unified-5h-Status":    "allowed",
				"Anthropic-Ratelimit-Unified-7d-Status":    "allowed",
				"Anthropic-Ratelimit-Unified-7d_oi-Status": "rejected",
				"Anthropic-Ratelimit-Unified-7d_oi-Reset":  in(3 * 24 * time.Hour),
			},
			want: limitModel, min: 3*24*time.Hour - time.Second, max: 3*24*time.Hour + resetJitter + time.Second,
		},
		{
			// Extra usage rejected; its reset is the billing-period boundary,
			// which says nothing about when the limit lifts.
			name: "extra usage rejection ignores the billing boundary",
			headers: map[string]string{
				"Anthropic-Ratelimit-Unified-Status":               "rejected",
				"Anthropic-Ratelimit-Unified-Representative-Claim": "overage",
				"Anthropic-Ratelimit-Unified-Overage-Status":       "rejected",
				"Anthropic-Ratelimit-Unified-Overage-Reset":        in(20 * 24 * time.Hour),
				"Anthropic-Ratelimit-Unified-Reset":                in(20 * 24 * time.Hour),
				"Anthropic-Ratelimit-Unified-5h-Utilization":       "0.4",
			},
			want: limitModel, min: exhaustedCooldown - time.Second, max: exhaustedCooldown + time.Second,
		},
		{
			// Orgs without extra usage carry an overage reason on every
			// response; a spent 5-hour window still limits the account.
			name: "claim naming a shared window limits the account",
			headers: map[string]string{
				"Anthropic-Ratelimit-Unified-Status":                  "rejected",
				"Anthropic-Ratelimit-Unified-Representative-Claim":    "five_hour",
				"Anthropic-Ratelimit-Unified-Overage-Disabled-Reason": "org_level_disabled",
				"Anthropic-Ratelimit-Unified-Reset":                   in(time.Hour),
			},
			want: limitAccount, min: time.Hour - time.Second, max: time.Hour + resetJitter + time.Second,
		},
		{
			name: "spent utilization limits the account",
			headers: map[string]string{
				"Anthropic-Ratelimit-Unified-Status":                  "rejected",
				"Anthropic-Ratelimit-Unified-Overage-Disabled-Reason": "org_level_disabled",
				"Anthropic-Ratelimit-Unified-5h-Utilization":          "1.0",
			},
			want: limitAccount, min: exhaustedCooldown - time.Second, max: exhaustedCooldown + time.Second,
		},
		{
			name:    "fast mode without usage credits is the request's",
			headers: map[string]string{"Anthropic-Ratelimit-Unified-Status": "allowed"},
			body:    `{"type":"error","error":{"type":"rate_limit_error","message":"Usage credits are required for fast mode."}}`,
			want:    limitFast,
		},
		{
			name: "bare 429 is the request's",
			body: `{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`,
			want: limitRequest,
		},
		{
			name:    "Retry-After is capped at a week",
			headers: map[string]string{"Retry-After": strconv.Itoa(30 * 24 * 3600)},
			want:    limitAccount, min: maxCooldown - time.Second, max: maxCooldown + time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tc.headers {
				h.Set(k, v)
			}
			scope, until := classify429(h, []byte(tc.body), now)
			if scope != tc.want {
				t.Fatalf("scope = %d, want %d", scope, tc.want)
			}
			if tc.want == limitAccount || tc.want == limitModel {
				if d := until.Sub(now); d < tc.min || d > tc.max {
					t.Fatalf("cooldown %v, want between %v and %v", d.Round(time.Second), tc.min, tc.max)
				}
			}
		})
	}
}

func TestRequestModel(t *testing.T) {
	for body, want := range map[string]string{
		`{"model":"claude-opus-5-5","messages":[]}`:             "claude-opus-5-5",
		`{"messages":[{"role":"user"}],"model":"claude-haiku"}`: "claude-haiku",
		`{"messages":[]}`: "",
		`not json`:        "",
		``:                "",
	} {
		if got := requestModel([]byte(body)); got != want {
			t.Errorf("requestModel(%q) = %q, want %q", body, got, want)
		}
	}
}

// modelLimited answers 429 with a per-model weekly limit for opus on
// account a, and 200 otherwise.
func modelLimited(w http.ResponseWriter, h hit) {
	if h.auth == "Bearer tokA" && strings.Contains(h.body, "opus") {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d_oi-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d_oi-Reset", strconv.FormatInt(time.Now().Add(48*time.Hour).Unix(), 10))
		w.WriteHeader(429)
		return
	}
	w.WriteHeader(200)
}

func TestModelLimitKeepsAccountForOtherModels(t *testing.T) {
	up := newFakeUpstream(t, modelLimited)
	sel := pool.NewSelector()
	accts := []pool.Account{{Name: "a", Token: "tokA"}, {Name: "b", Token: "tokB"}}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
	ts := newProxy(t, up.URL, testRuntime(accts, clients), "", func(o *Options) { o.Selector = sel })

	opus, sonnet := `{"model":"claude-opus-5-5"}`, `{"model":"claude-sonnet-5-5"}`
	if resp := doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), opus); resp.StatusCode != 200 {
		t.Fatalf("opus request: %d, want 200 from b", resp.StatusCode)
	}
	st := sel.StatsSnapshot([]string{"a"})["a"]
	if !st.CoolingUntil.IsZero() {
		t.Fatalf("account a benched for every model: cooling until %v", st.CoolingUntil)
	}
	if until := st.ModelCooling["claude-opus-5-5"]; time.Until(until) < 47*time.Hour {
		t.Fatalf("opus on a cooling until %v, want ~48h", until)
	}

	// Opus now goes straight to b; a still serves sonnet for a new session.
	before := len(up.recorded())
	if resp := doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), opus); resp.StatusCode != 200 {
		t.Fatalf("second opus request: %d", resp.StatusCode)
	}
	if hits := up.recorded()[before:]; len(hits) != 1 || hits[0].auth != "Bearer tokB" {
		t.Fatalf("second opus request hit %v, want only b", up.auths()[before:])
	}
	if ranked := sel.Ranked(accts, pool.StrategyRoundRobin, pool.Route{Pool: "default", Model: "claude-sonnet-5-5"}); len(ranked) != 2 {
		t.Fatalf("sonnet candidates = %v, want both accounts", ranked)
	}
	if resp := doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), sonnet); resp.StatusCode != 200 {
		t.Fatalf("sonnet request: %d", resp.StatusCode)
	}
}

func TestFastCreditsRefusalIsPassedThrough(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.2")
		w.WriteHeader(429)
		io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"Usage credits are required for fast mode."}}`)
	})
	sel := pool.NewSelector()
	accts := []pool.Account{{Name: "a", Token: "tokA"}, {Name: "b", Token: "tokB"}}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
	ts := newProxy(t, up.URL, testRuntime(accts, clients), "", func(o *Options) { o.Selector = sel })

	resp := doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), `{"model":"m","speed":"fast"}`)
	if resp.StatusCode != 429 {
		t.Fatalf("got %d, want the upstream 429", resp.StatusCode)
	}
	if b, _ := io.ReadAll(resp.Body); !strings.Contains(string(b), "fast mode") {
		t.Fatalf("client got %s, want the upstream body", b)
	}
	if n := len(up.recorded()); n != 1 {
		t.Fatalf("upstream saw %d attempts, want 1 (no pool walk)", n)
	}
	for name, st := range sel.StatsSnapshot([]string{"a", "b"}) {
		if !st.CoolingUntil.IsZero() || st.RateLimited != 0 {
			t.Fatalf("account %s benched by a fast-mode refusal: %+v", name, st)
		}
	}
}

func TestForbiddenFailsOver(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
		if h.auth == "Bearer tokA" || h.body == "nobody" {
			w.WriteHeader(403)
			io.WriteString(w, `{"type":"error","error":{"type":"permission_error","message":"This organization has been disabled."}}`)
			return
		}
		w.WriteHeader(200)
	})
	sel := pool.NewSelector()
	accts := []pool.Account{{Name: "a", Token: "tokA"}, {Name: "b", Token: "tokB"}}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
	ts := newProxy(t, up.URL, testRuntime(accts, clients), "", func(o *Options) { o.Selector = sel })

	// A 403 every account gets is the request's: surfaced, nobody benched.
	if resp := doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), "nobody"); resp.StatusCode != 403 {
		t.Fatalf("request every account refuses: %d, want 403", resp.StatusCode)
	}
	for name, st := range sel.StatsSnapshot([]string{"a", "b"}) {
		if !st.CoolingUntil.IsZero() {
			t.Fatalf("account %s benched by a 403 every account got: %+v", name, st)
		}
	}

	// A 403 only a gets: served by b, a benched.
	for range 2 { // whichever account the client starts on
		if resp := doReq(t, "POST", ts.URL+"/v1/messages", aliceHeaders(), "{}"); resp.StatusCode != 200 {
			t.Fatalf("request: %d, want 200 from b", resp.StatusCode)
		}
	}
	st := sel.StatsSnapshot([]string{"a"})["a"]
	if d := time.Until(st.CoolingUntil); d < forbiddenCooldown-time.Minute || st.Rejected != 1 {
		t.Fatalf("a after its 403: cooling %v, rejected %d; want ~%v, 1", d, st.Rejected, forbiddenCooldown)
	}
}

func TestSessionsPinIndependently(t *testing.T) {
	// A bare 429 on a for session one fails it over to b without benching
	// a; session two keeps its own pin.
	up := newFakeUpstream(t, func(w http.ResponseWriter, h hit) {
		if h.auth == "Bearer tokA" && h.body == "refuse" {
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(200)
	})
	sel := pool.NewSelector()
	accts := []pool.Account{{Name: "a", Token: "tokA"}, {Name: "b", Token: "tokB"}}
	clients := map[string]config.ClientAuth{"cp_alice": {Name: "alice"}}
	ts := newProxy(t, up.URL, testRuntime(accts, clients), "", func(o *Options) { o.Selector = sel })

	session := func(id string) map[string]string {
		h := aliceHeaders()
		h["X-Claude-Code-Session-Id"] = id
		return h
	}
	one := pool.Route{Pool: config.DefaultPoolName, Client: "alice", Session: "s1"}
	two := pool.Route{Pool: config.DefaultPoolName, Client: "alice", Session: "s2"}
	sel.Pin(one, "a")
	sel.Pin(two, "a")
	if resp := doReq(t, "POST", ts.URL+"/v1/messages", session("s1"), "refuse"); resp.StatusCode != 200 {
		t.Fatalf("session one: %d, want 200 from b", resp.StatusCode)
	}
	if got := sel.Assignment(one); got != "b" {
		t.Fatalf("session one pinned to %q, want b after failing over", got)
	}
	if got := sel.Assignment(two); got != "a" {
		t.Fatalf("session two pinned to %q, want a: one session's failover moved another", got)
	}
	if got := sel.Assignment(pool.Route{Pool: config.DefaultPoolName, Client: "alice"}); got != "" {
		t.Fatalf("client-level pin %q set by session traffic", got)
	}

	// The usage JSON lists both of the client's accounts and the sessions.
	snap := usageSnapshot(t, ts)
	var cli UsageClient
	for _, c := range snap.Clients {
		if c.Name == "alice" {
			cli = c
		}
	}
	if cli.Pinned != "b, a" {
		t.Fatalf("alice pinned = %q, want %q (most recent first)", cli.Pinned, "b, a")
	}
	for _, a := range snap.Accounts {
		if a.Sessions != 1 {
			t.Fatalf("account %s has %d sessions, want 1", a.Name, a.Sessions)
		}
	}
}

func usageSnapshot(t *testing.T, ts *httptest.Server) UsageSnapshot {
	t.Helper()
	resp := doReq(t, "GET", ts.URL+"/usage?format=json", aliceHeaders(), "")
	var snap UsageSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	return snap
}
