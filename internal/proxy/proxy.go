// Package proxy implements the Anthropic-compatible reverse proxy: client
// authentication, upstream credential rewriting, sticky per-client account
// assignment and transparent rate-limit failover.
package proxy

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dehydr8/cproxy/internal/config"
	"github.com/dehydr8/cproxy/internal/pool"
)

// Options configures one proxy server.
type Options struct {
	Upstream  *url.URL          // required: upstream base URL
	State     *State            // required: initial request-time state
	Selector  *pool.Selector    // shared health state; created if nil
	MaxBody   int64             // max replayable request body; default 32 MiB
	Log       *slog.Logger      // default slog.Default()
	Transport http.RoundTripper // override for tests

	// HideUsage keeps the dashboard (/usage) and /_cproxy/* off this
	// handler; serve them on a private listener with UsageHandler instead.
	HideUsage bool
}

// State is the request-time configuration. Serve mode swaps it atomically
// on reload (SIGHUP); run mode sets it once.
type State struct {
	Runtime      *config.Runtime
	SessionToken string // run mode: the single generated client token
	NoAuth       bool   // serve --no-auth: accept any client (dangerous)
}

const defaultMaxBody = 32 << 20 // 32 MiB: comfortably above any Claude request

var hopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// Server is the cproxy HTTP handler.
type Server struct {
	upstream  *url.URL
	state     atomic.Pointer[State]
	sel       *pool.Selector
	client    *http.Client
	log       *slog.Logger
	maxBody   int64
	started   time.Time // for the usage dashboard's uptime
	hideUsage bool

	ephemeral sync.Map // client token -> config.ClientAuth registered at runtime (cproxy run via the daemon)

	usage       *usagePoller // real usage API cache (preferred usage source)
	usageStop   chan struct{}
	usageWarm   chan struct{} // closed after the poller's first cycle
	usageViewed atomic.Int64  // unix nanos of the last usage view
	usageOnce   sync.Once
	closeOnce   sync.Once
}

// New builds a proxy server from opts.
func New(opts Options) *Server {
	if opts.MaxBody <= 0 {
		opts.MaxBody = defaultMaxBody
	}
	if opts.Selector == nil {
		opts.Selector = pool.NewSelector()
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.State == nil {
		opts.State = &State{Runtime: &config.Runtime{}}
	}
	tr := opts.Transport
	if tr == nil {
		tr = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			// Without a dial timeout an unreachable upstream holds the request
			// for the OS connect timeout (minutes) instead of failing over.
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}
	s := &Server{
		upstream:  opts.Upstream,
		sel:       opts.Selector,
		client:    &http.Client{Transport: tr},
		log:       opts.Log,
		maxBody:   opts.MaxBody,
		started:   time.Now(),
		hideUsage: opts.HideUsage,
		usage: &usagePoller{
			entries: map[string]*usageEntry{},
		},
		usageStop: make(chan struct{}),
		usageWarm: make(chan struct{}),
	}
	s.state.Store(opts.State)
	return s
}

// Update swaps the request-time state (serve-mode reload).
func (s *Server) Update(st *State) { s.state.Store(st) }

// Selector exposes the shared selector so reloads can clear stale health.
func (s *Server) Selector() *pool.Selector { return s.sel }

// ForgetAccount drops an account's health state and cached usage API result,
// e.g. after a reload replaced its token: a 401 or a 403 scope error from the
// old token must not stick to the new one.
func (s *Server) ForgetAccount(name string) {
	s.sel.ClearAccount(name)
	s.usage.forget(name)
}

// AddClient registers a client token for as long as this process runs (or
// until RemoveClient), independent of configuration reloads. The local
// daemon gives every claude and `cproxy run` its own, so each gets its own
// sticky pin and its own row on the dashboard.
func (s *Server) AddClient(token string, ca config.ClientAuth) { s.ephemeral.Store(token, ca) }

// RemoveClient drops a registered client and forgets its stats and pins.
func (s *Server) RemoveClient(token string) (config.ClientAuth, bool) {
	v, ok := s.ephemeral.LoadAndDelete(token)
	if !ok {
		return config.ClientAuth{}, false
	}
	ca := v.(config.ClientAuth)
	s.sel.ForgetClient(ca.Name)
	return ca, true
}

// Clients returns the registered (runtime) clients by token.
func (s *Server) Clients() map[string]config.ClientAuth {
	out := map[string]config.ClientAuth{}
	s.ephemeral.Range(func(k, v any) bool {
		out[k.(string)] = v.(config.ClientAuth)
		return true
	})
	return out
}

// authenticate resolves the client of a request: configured tokens first,
// then runtime-registered ones.
func (s *Server) authenticate(st *State, r *http.Request) (config.ClientAuth, bool) {
	if ca, ok := st.authenticate(r); ok {
		return ca, true
	}
	if tok := clientToken(r); tok != "" {
		if v, ok := s.ephemeral.Load(tok); ok {
			return v.(config.ClientAuth), true
		}
	}
	return config.ClientAuth{}, false
}

func (st *State) authenticate(r *http.Request) (config.ClientAuth, bool) {
	if st.NoAuth {
		return config.ClientAuth{Name: "anonymous"}, true
	}
	tok := clientToken(r)
	if tok == "" && r.URL.Path == "/usage" {
		// Browsers cannot set Authorization headers; let the dashboard be
		// opened as /usage?token=<client token>. API paths stay header-only.
		tok = strings.TrimSpace(r.URL.Query().Get("token"))
	}
	if tok == "" {
		return config.ClientAuth{}, false
	}
	if st.SessionToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(st.SessionToken)) == 1 {
		return config.ClientAuth{Name: "local"}, true
	}
	if st.Runtime != nil {
		if ca, ok := st.Runtime.ClientAuth[tok]; ok {
			return ca, true
		}
	}
	return config.ClientAuth{}, false
}

// clientToken extracts the client credential from Authorization (Bearer or
// raw) or x-api-key.
func clientToken(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if h != "" {
		if len(h) >= 7 && strings.EqualFold(h[:7], "bearer ") {
			return strings.TrimSpace(h[7:])
		}
		return h
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

// ServeHTTP serves the proxy API and, unless HideUsage is set, the usage
// dashboard.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.serve(w, r, !s.hideUsage, true)
}

// UsageHandler serves only the usage dashboard and /_cproxy/*, for a private
// listener (serve --usage-addr) next to a public API-only one.
func (s *Server) UsageHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.serve(w, r, true, false)
	})
}

func isUsagePath(p string) bool { return p == "/usage" || strings.HasPrefix(p, "/_cproxy/") }

func (s *Server) serve(w http.ResponseWriter, r *http.Request, usage, api bool) {
	start := time.Now()
	st := s.state.Load()

	// Paths this listener does not serve are 404 before authentication,
	// so a public API-only listener does not reveal the dashboard exists.
	if isUsagePath(r.URL.Path) && !usage || !isUsagePath(r.URL.Path) && !api {
		writeError(w, http.StatusNotFound, "not_found_error", "cproxy: not found")
		return
	}

	if r.URL.Path == WhoamiPath && r.Method == http.MethodGet {
		s.serveWhoami(w, r, st) // unauthenticated by design; see WhoamiPath
		return
	}

	ca, ok := s.authenticate(st, r)
	if !ok {
		s.log.Warn("rejected client", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		writeError(w, http.StatusUnauthorized, "authentication_error",
			"cproxy: invalid or missing client token (set ANTHROPIC_AUTH_TOKEN to the cproxy token you were given)")
		return
	}

	if r.URL.Path == "/usage" {
		if !ca.HasScope(config.ScopeUsage) {
			writeError(w, http.StatusForbidden, "permission_error",
				"cproxy: this token may not view usage (add the \"usage\" scope)")
			return
		}
		s.serveUsage(w, r, st)
		return
	}
	if !ca.HasScope(config.ScopeProxy) {
		writeError(w, http.StatusForbidden, "permission_error",
			"cproxy: this token may not use the proxy (usage-scoped tokens can only view /usage)")
		return
	}

	members, strategy, err := st.Runtime.ResolvePool(ca.Pool)
	if err != nil {
		s.log.Error("pool resolution failed", "client", ca.Name, "err", err.Error())
		writeError(w, http.StatusServiceUnavailable, "api_error", "cproxy: "+err.Error())
		return
	}
	s.sel.MarkClientAttempt(ca.Name)
	defer s.sel.BeginClient(ca.Name)()

	body, replay, err := s.readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"cproxy: could not read request body: "+err.Error())
		return
	}

	pn := st.Runtime.PoolFor(ca)
	// Read the pin before Ranked, which remaps a client whose pinned account
	// became unavailable.
	prev := s.sel.Assignment(pn, ca.Name)
	ranked := s.sel.Ranked(members, strategy, pn, ca.Name)
	if len(ranked) == 0 {
		s.rejectAllUnavailable(w, members, pn, ca)
		return
	}
	if prev != "" && prev != ranked[0].Name {
		s.log.Warn("switching account", "client", ca.Name, "pool", pn,
			"from", prev, "to", ranked[0].Name, "reason", s.sel.SwitchReason(pn, ca.Name))
	}

	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	var held *heldResponse
	var lastErr error
	// Each attempt counts as in flight on its account until it is done; the
	// deferred sweep covers every exit (the end funcs are idempotent).
	var ends []func()
	defer func() {
		for _, end := range ends {
			end()
		}
	}()
	for _, acct := range ranked {
		end := s.sel.Begin(acct.Name)
		ends = append(ends, end)
		resp, err := s.forward(r, body, replay, acct)
		if err != nil {
			end()
		}
		if err != nil {
			if r.Context().Err() != nil {
				// The client went away (e.g. interrupted in Claude); that says
				// nothing about the account, and every further attempt would
				// fail the same way and bench the whole pool.
				s.log.Info("client canceled request", "client", ca.Name, "account", acct.Name)
				return
			}
			lastErr = err
			s.log.Warn("upstream attempt failed", "account", acct.Name, "err", err.Error())
			s.sel.MarkError(acct.Name, time.Now().Add(15*time.Second))
			if !replay {
				break // the streamed body is (partly) consumed; it cannot be resent
			}
			continue
		}

		// Capture any rate-limit headers the upstream sent; we are the
		// proxy, so this is the only place quota state is visible.
		if rl := parseRateLimits(resp.Header); rl != nil {
			s.sel.MarkRateLimits(acct.Name, *rl)
		}

		if resp.StatusCode == http.StatusUnauthorized {
			// The upstream rejected this credential outright.
			s.sel.MarkInvalid(acct.Name)
			s.log.Warn("upstream rejected token", "account", acct.Name, "status", resp.StatusCode)
			if !replay {
				s.copyResponse(rec, resp, nil)
				s.logDone(r, rec, ca, pn, acct.Name, start)
				return
			}
			drainClose(resp)
			end()
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests && !isRateLimit(resp.Header) {
			// A refusal of this request, not a limit on the account: try the
			// others (an API-key member may serve it) without benching any.
			s.log.Info("upstream refused the request with a bare 429; not benching the account",
				"account", acct.Name, "can_retry", replay)
			if !replay {
				s.copyResponse(rec, resp, nil)
				s.logDone(r, rec, ca, pn, acct.Name, start)
				return
			}
			held = captureHeld(resp)
			end()
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			until := rateLimitedUntil(resp)
			s.sel.MarkRateLimited(acct.Name, until)
			s.log.Warn("account rate limited", "account", acct.Name,
				"cooldown", until.Sub(time.Now()).Round(time.Second), "can_retry", replay)
			if !replay {
				s.copyResponse(rec, resp, nil)
				s.logDone(r, rec, ca, pn, acct.Name, start)
				return
			}
			held = captureHeld(resp)
			end()
			continue
		}

		s.sel.MarkSuccess(acct.Name)
		if strategy == pool.StrategySticky && acct.Name != ranked[0].Name {
			// Failed over: keep the client on the account that now holds its
			// conversation instead of remapping it again next request.
			s.sel.Pin(pn, ca.Name, acct.Name)
		}
		var onUsage func(pool.TokenUsage)
		if strings.HasSuffix(r.URL.Path, "/v1/messages") {
			name, client := acct.Name, ca.Name
			onUsage = func(u pool.TokenUsage) { s.sel.AddTokens(name, client, u) }
		}
		s.copyResponse(rec, resp, onUsage)
		s.logDone(r, rec, ca, pn, acct.Name, start)
		return
	}

	if held != nil {
		// Every usable account was tried; surface the last upstream 429.
		s.writeHeld(rec, held)
		s.logDone(r, rec, ca, pn, "exhausted", start)
		return
	}
	s.log.Error("all upstream attempts failed", "client", ca.Name, "pool", pn, "err", errString(lastErr))
	writeError(rec, http.StatusBadGateway, "api_error",
		"cproxy: all upstream attempts failed; last error: "+errString(lastErr))
}

// readBody returns the request body bytes when they fit under the replay
// limit, which makes failover retries safe. Larger bodies are streamed
// through as-is with replay=false (single attempt, no retry).
func (s *Server) readBody(r *http.Request) (b []byte, replay bool, err error) {
	if r.Body == nil || r.ContentLength == 0 {
		return nil, true, nil
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, s.maxBody+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(buf)) > s.maxBody {
		s.log.Warn("request body exceeds replay limit; forwarding without failover",
			"bytes", len(buf), "limit", s.maxBody)
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), r.Body))
		return nil, false, nil
	}
	return buf, true, nil
}

// forward performs one upstream attempt presenting the given account.
func (s *Server) forward(r *http.Request, body []byte, replay bool, acct pool.Account) (*http.Response, error) {
	u := *r.URL
	u.Scheme = s.upstream.Scheme
	u.Host = s.upstream.Host
	if p := s.upstream.Path; p != "" {
		u.Path = strings.TrimSuffix(p, "/") + u.Path
	}

	var rd io.Reader
	if replay {
		rd = bytes.NewReader(body)
	} else {
		rd = r.Body
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), rd)
	if err != nil {
		return nil, err
	}
	copyHeaders(req.Header, r.Header)

	// Replace the client's credentials with the selected account's.
	req.Header.Del("Authorization")
	req.Header.Del("X-Api-Key")
	switch acct.Type {
	case pool.TokenTypeAPI:
		req.Header.Set("X-Api-Key", acct.Token)
	default:
		req.Header.Set("Authorization", "Bearer "+acct.Token)
	}
	if replay {
		req.ContentLength = int64(len(body))
	}
	return s.client.Do(req)
}

// copyHeaders copies client headers, dropping hop-by-hop headers, the
// client's credentials and framing (recomputed for the upstream request).
func copyHeaders(dst, src http.Header) {
	for k, vals := range src {
		if slices.Contains(hopHeaders, k) || k == "Content-Length" ||
			k == "Authorization" || k == "X-Api-Key" {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

// copyResponse streams the upstream response through, flushing per chunk so
// SSE events reach the client promptly.
// onUsage, when set, receives the token usage found in a 200 response.
func (s *Server) copyResponse(w http.ResponseWriter, resp *http.Response, onUsage func(pool.TokenUsage)) {
	defer resp.Body.Close()
	dst := w.Header()
	for k, vals := range resp.Header {
		if slices.Contains(hopHeaders, k) {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	var body io.Reader = resp.Body
	finish := func() {}
	if onUsage != nil && resp.StatusCode == http.StatusOK {
		body, finish = tapUsage(resp, onUsage)
	}
	flushCopy(w, body)
	finish()
}

func flushCopy(w io.Writer, body io.Reader) {
	buf := make([]byte, 32*1024)
	fl, canFlush := w.(http.Flusher)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if canFlush {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// heldResponse is a small buffered upstream response (a 429) kept in case
// every failover attempt is exhausted.
type heldResponse struct {
	status int
	header http.Header
	body   []byte
}

func captureHeld(resp *http.Response) *heldResponse {
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return &heldResponse{resp.StatusCode, resp.Header.Clone(), b}
}

func (s *Server) writeHeld(w http.ResponseWriter, held *heldResponse) {
	dst := w.Header()
	for k, vals := range held.header {
		if slices.Contains(hopHeaders, k) {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
	w.WriteHeader(held.status)
	w.Write(held.body)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

func drainClose(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

// retryAfter parses Retry-After (delta-seconds or HTTP-date), falling back to
// def. Cooldowns are capped at 24h.
func retryAfter(resp *http.Response, def time.Duration) time.Time {
	now := time.Now()
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return now.Add(def)
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			secs = 0
		}
		if secs > 86400 {
			secs = 86400
		}
		return now.Add(time.Duration(secs) * time.Second)
	}
	if t, err := http.ParseTime(v); err == nil {
		if t.Before(now) {
			return now
		}
		if t.After(now.Add(24 * time.Hour)) {
			return now.Add(24 * time.Hour)
		}
		return t
	}
	return now.Add(def)
}

// isRateLimit reports whether a 429 is an actual rate limit, i.e. carries
// Retry-After or anthropic-ratelimit-* headers. OAuth tokens also get a bare
// 429 (rate_limit_error "Error", neither header) for requests Anthropic will
// not serve on them, such as ones without Claude Code's system prompt; that
// says nothing about the account, which keeps serving normal requests.
func isRateLimit(h http.Header) bool {
	if strings.TrimSpace(h.Get("Retry-After")) != "" {
		return true
	}
	for k := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "Anthropic-Ratelimit-") {
			return true
		}
	}
	return false
}

// Cooldowns after a 429.
const (
	// exhaustedCooldown applies when the subscription reports itself
	// exhausted but sends no reset time.
	exhaustedCooldown = 10 * time.Minute
	// briefCooldown applies to any other 429 without Retry-After: a
	// short-term or request-specific limit says little about the account,
	// so it must not bench it for long.
	briefCooldown = 30 * time.Second
)

// rateLimitedUntil decides how long a 429'd account cools down:
//
//   - the subscription itself is exhausted (unified-status "rejected"):
//     until anthropic-ratelimit-unified-reset, else Retry-After, else
//     exhaustedCooldown;
//   - any other 429: Retry-After when present, else briefCooldown.
//
// Cooldowns are capped at 24h.
func rateLimitedUntil(resp *http.Response) time.Time {
	now := time.Now()
	hasRetryAfter := strings.TrimSpace(resp.Header.Get("Retry-After")) != ""
	if strings.EqualFold(strings.TrimSpace(resp.Header.Get("Anthropic-Ratelimit-Unified-Status")), "rejected") {
		if t, ok := headerReset(resp.Header, "Anthropic-Ratelimit-Unified-Reset"); ok && t.After(now) {
			if limit := now.Add(24 * time.Hour); t.After(limit) {
				return limit
			}
			return t
		}
		if hasRetryAfter {
			return retryAfter(resp, exhaustedCooldown)
		}
		return now.Add(exhaustedCooldown)
	}
	if hasRetryAfter {
		return retryAfter(resp, briefCooldown)
	}
	return now.Add(briefCooldown)
}

// parseRateLimits extracts an Anthropic rate-limit snapshot from response
// headers, returning nil when the upstream sent no rate-limit headers.
func parseRateLimits(h http.Header) *pool.RateLimitInfo {
	info := &pool.RateLimitInfo{Status: strings.TrimSpace(h.Get("Anthropic-Ratelimit-Unified-Status"))}
	if v, ok := headerInt(h, "Anthropic-Ratelimit-Requests-Limit"); ok {
		info.RequestsLimit = &v
	}
	if v, ok := headerInt(h, "Anthropic-Ratelimit-Requests-Remaining"); ok {
		info.RequestsRemaining = &v
	}
	if t, ok := headerReset(h, "Anthropic-Ratelimit-Requests-Reset"); ok {
		info.RequestsReset = &t
	}
	if v, ok := headerInt(h, "Anthropic-Ratelimit-Tokens-Limit"); ok {
		info.TokensLimit = &v
	}
	if v, ok := headerInt(h, "Anthropic-Ratelimit-Tokens-Remaining"); ok {
		info.TokensRemaining = &v
	}
	if t, ok := headerReset(h, "Anthropic-Ratelimit-Tokens-Reset"); ok {
		info.TokensReset = &t
	}
	// Subscription windows report utilization as a 0–1 fraction.
	if v, ok := headerFraction(h, "Anthropic-Ratelimit-Unified-5h-Utilization"); ok {
		info.FiveHourPct = &v
	}
	if t, ok := headerReset(h, "Anthropic-Ratelimit-Unified-5h-Reset"); ok {
		info.FiveHourReset = &t
	}
	if v, ok := headerFraction(h, "Anthropic-Ratelimit-Unified-7d-Utilization"); ok {
		info.SevenDayPct = &v
	}
	if t, ok := headerReset(h, "Anthropic-Ratelimit-Unified-7d-Reset"); ok {
		info.SevenDayReset = &t
	}
	info.Claim = strings.TrimSpace(h.Get("Anthropic-Ratelimit-Unified-Representative-Claim"))
	info.OverageStatus = strings.TrimSpace(h.Get("Anthropic-Ratelimit-Unified-Overage-Status"))
	info.OverageReason = strings.TrimSpace(h.Get("Anthropic-Ratelimit-Unified-Overage-Disabled-Reason"))
	if info.Status == "" && info.RequestsLimit == nil && info.RequestsRemaining == nil &&
		info.RequestsReset == nil && info.TokensLimit == nil && info.TokensRemaining == nil && info.TokensReset == nil &&
		info.FiveHourPct == nil && info.SevenDayPct == nil {
		return nil
	}
	info.Seen = time.Now()
	return info
}

// headerInt reads an integer rate-limit header.
func headerInt(h http.Header, key string) (int64, bool) {
	v := strings.TrimSpace(h.Get(key))
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// headerFraction reads a 0–1 utilization header as a percentage.
func headerFraction(h http.Header, key string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(h.Get(key)), 64)
	if err != nil {
		return 0, false
	}
	return math.Round(v*1000) / 10, true // one decimal, like the usage API
}

// headerReset parses a reset header. Anthropic sends RFC3339 timestamps;
// delta-seconds, Go durations and epoch timestamps are accepted too.
func headerReset(h http.Header, key string) (time.Time, bool) {
	v := strings.TrimSpace(h.Get(key))
	if v == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return time.Now().Add(d), true
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 86400*366 {
			return time.Now().Add(time.Duration(n) * time.Second), true
		}
		return time.Unix(n, 0), true
	}
	return time.Time{}, false
}

// rejectAllUnavailable answers with an Anthropic-style 429 when every pool
// member is cooling down or invalid, so Claude's native retry UX kicks in.
func (s *Server) rejectAllUnavailable(w http.ResponseWriter, members []pool.Account, pn string, ca config.ClientAuth) {
	until := s.sel.NextRecovery(members)
	if until.IsZero() {
		writeError(w, http.StatusTooManyRequests, "rate_limit_error",
			fmt.Sprintf("cproxy: every account in pool %q has a rejected (invalid) token; check `cproxy accounts list`", pn))
		s.log.Error("no available account: all tokens rejected", "pool", pn, "client", ca.Name)
		return
	}
	secs := int(time.Until(until).Seconds()) + 1
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeError(w, http.StatusTooManyRequests, "rate_limit_error",
		fmt.Sprintf("cproxy: every account in pool %q is cooling down after rate limits; retry later", pn))
	s.log.Warn("no available account", "pool", pn, "client", ca.Name, "recovers", until.Format(time.RFC3339))
}

// anthropicError is the error body shape Anthropic APIs return, so Claude
// renders cproxy errors like its own.
type anthropicError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	var e anthropicError
	e.Type = "error"
	e.Error.Type = typ
	e.Error.Message = msg
	b, err := json.Marshal(e)
	if err != nil {
		http.Error(w, msg, status)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

// recorder captures the response status for logging.
type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards Flush so SSE streams through the recorder.
func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logDone(r *http.Request, rec *recorder, ca config.ClientAuth, pn, acct string, start time.Time) {
	s.log.Info("proxied",
		"account", acct, "client", ca.Name, "pool", pn,
		"method", r.Method, "path", r.URL.Path,
		"status", rec.status, "dur", time.Since(start).Round(time.Millisecond))
}

func errString(err error) string {
	if err == nil {
		return "unknown"
	}
	return err.Error()
}
