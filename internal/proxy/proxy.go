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
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
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
	upstream *url.URL
	state    atomic.Pointer[State]
	sel      *pool.Selector
	client   *http.Client
	log      *slog.Logger
	maxBody  int64
	started  time.Time // for the usage dashboard's uptime
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
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}
	s := &Server{
		upstream: opts.Upstream,
		sel:      opts.Selector,
		client:   &http.Client{Transport: tr},
		log:      opts.Log,
		maxBody:  opts.MaxBody,
		started:  time.Now(),
	}
	s.state.Store(opts.State)
	return s
}

// Update swaps the request-time state (serve-mode reload).
func (s *Server) Update(st *State) { s.state.Store(st) }

// Selector exposes the shared selector so reloads can clear stale health.
func (s *Server) Selector() *pool.Selector { return s.sel }

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

func poolNameFor(ca config.ClientAuth) string {
	if ca.Pool != "" {
		return ca.Pool
	}
	return config.DefaultPoolName
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	st := s.state.Load()

	ca, ok := st.authenticate(r)
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

	body, replay, err := s.readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"cproxy: could not read request body: "+err.Error())
		return
	}

	pn := poolNameFor(ca)
	ranked := s.sel.Ranked(members, strategy, pn, ca.Name)
	if len(ranked) == 0 {
		s.rejectAllUnavailable(w, members, pn, ca)
		return
	}
	if prev := s.sel.Assignment(pn, ca.Name); prev != "" && prev != ranked[0].Name {
		s.log.Warn("switching account", "client", ca.Name, "pool", pn,
			"from", prev, "to", ranked[0].Name, "reason", "previous account unavailable")
	}

	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	var held *heldResponse
	var lastErr error
	for _, acct := range ranked {
		resp, err := s.forward(r, body, replay, acct)
		if err != nil {
			lastErr = err
			s.log.Warn("upstream attempt failed", "account", acct.Name, "err", err.Error())
			s.sel.MarkError(acct.Name, time.Now().Add(15*time.Second))
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
				s.copyResponse(rec, resp)
				s.logDone(r, rec, ca, pn, acct.Name, start)
				return
			}
			drainClose(resp)
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			until := retryAfter(resp, 10*time.Minute)
			s.sel.MarkRateLimited(acct.Name, until)
			s.log.Warn("account rate limited", "account", acct.Name,
				"cooldown", until.Sub(time.Now()).Round(time.Second), "can_retry", replay)
			if !replay {
				s.copyResponse(rec, resp)
				s.logDone(r, rec, ca, pn, acct.Name, start)
				return
			}
			held = captureHeld(resp)
			continue
		}

		s.sel.MarkSuccess(acct.Name)
		s.copyResponse(rec, resp)
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
func (s *Server) copyResponse(w http.ResponseWriter, resp *http.Response) {
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
	flushCopy(w, resp.Body)
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
	if info.Status == "" && info.RequestsLimit == nil && info.RequestsRemaining == nil &&
		info.RequestsReset == nil && info.TokensLimit == nil && info.TokensRemaining == nil && info.TokensReset == nil {
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
