// Package pool implements credential-pool membership, selection strategies
// and per-account health state (rate-limit cooldowns, rejected tokens).
package pool

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// TokenType classifies an Anthropic secret and decides which upstream header
// cproxy rewrites it into.
type TokenType string

const (
	// TokenTypeOAuth secrets (sk-ant-oat01-…) are sent as Authorization: Bearer.
	TokenTypeOAuth TokenType = "oauth"
	// TokenTypeAPI keys (sk-ant-api03-…) are sent as x-api-key.
	TokenTypeAPI TokenType = "api"
)

// Account is one Anthropic credential cproxy can present upstream.
type Account struct {
	Name     string    `json:"name"`
	Token    string    `json:"token"`
	Type     TokenType `json:"type,omitempty"`
	Pools    []string  `json:"pools,omitempty"`
	Disabled bool      `json:"disabled,omitempty"`
	Source   string    `json:"-"` // "file" or "env"; informational only
}

// DetectTokenType classifies a secret by its well-known prefix. Unrecognized
// secrets are treated as OAuth bearer tokens.
func DetectTokenType(token string) TokenType {
	if strings.HasPrefix(token, "sk-ant-api") {
		return TokenTypeAPI
	}
	return TokenTypeOAuth
}

// MaskToken renders a secret safe for terminal display.
func MaskToken(tok string) string {
	if len(tok) <= 12 {
		return strings.Repeat("*", len(tok))
	}
	return tok[:min(14, len(tok)-4)] + "…" + tok[len(tok)-4:]
}

// Strategy decides the order pool members are tried in.
type Strategy string

const (
	// StrategySticky pins each Claude Code session (or, for requests
	// without a session id, each client) to one account and only moves it
	// when that account is rate-limited or rejected. It is the default: a
	// conversation that stays on one account pays its full context there
	// exactly once, instead of every pool member paying for the same context.
	StrategySticky Strategy = "sticky"
	// StrategyRoundRobin rotates across members on every request.
	StrategyRoundRobin Strategy = "round-robin"
	// StrategyLeastUsed prefers the account with the fewest served requests.
	StrategyLeastUsed Strategy = "least-used"
	// StrategyRandom shuffles members on every request.
	StrategyRandom Strategy = "random"
)

// DefaultStrategy is used when a pool defines none.
const DefaultStrategy = StrategySticky

var allStrategies = []string{
	string(StrategySticky),
	string(StrategyRoundRobin),
	string(StrategyLeastUsed),
	string(StrategyRandom),
}

// ParseStrategy validates a strategy name, defaulting to sticky when empty.
func ParseStrategy(s string) (Strategy, error) {
	if s == "" {
		return DefaultStrategy, nil
	}
	for _, v := range allStrategies {
		if v == s {
			return Strategy(s), nil
		}
	}
	return "", fmt.Errorf("unknown strategy %q (want one of: %s)", s, Strategies())
}

// Strategies returns the accepted strategy names for help text.
func Strategies() string { return strings.Join(allStrategies, ", ") }

// SessionTTL is how long an idle session keeps its pin. It matches the
// longest prompt cache TTL: once a session has been idle that long its cache
// is gone, so moving it costs nothing.
const SessionTTL = time.Hour

// Route says who a request is for: the pool and client it came through,
// the Claude Code session it belongs to (X-Claude-Code-Session-Id; empty
// for other clients) and the model it asks for (empty when unknown).
type Route struct {
	Pool, Client, Session, Model string
}

// PinKey identifies one sticky pin: a session of a client, or the client
// itself when its requests carry no session id.
type PinKey struct {
	Pool, Client, Session string
}

func (r Route) key() PinKey { return PinKey{r.Pool, r.Client, r.Session} }

// Pin is one sticky assignment, for the usage page.
type Pin struct {
	PinKey
	Account string
	Seen    time.Time // last request routed through it
}

// Selector tracks sticky assignments, request counters and per-account health
// for the pools served by one proxy process. All methods are safe for
// concurrent use; health state survives serve-mode config reloads.
type Selector struct {
	mu          sync.Mutex
	rr          map[string]int       // pool -> round-robin cursor for new assignments
	served      map[string]int64     // account -> requests served
	rateLimited map[string]int64     // account -> 429 responses seen
	rejected    map[string]int64     // account -> 401 and 403 responses seen
	upstreamErr map[string]int64     // account -> transport errors seen
	lastUsed    map[string]time.Time // account -> last request served
	clientReq   map[string]int64     // client -> accepted requests
	clientLast  map[string]time.Time // client -> last accepted request
	rateLimits  map[string]*RateLimitInfo
	sticky      map[PinKey]string               // pin -> account name
	pinSeen     map[PinKey]time.Time            // pin -> last request routed through it
	lastPrune   time.Time                       // last sweep of idle session pins
	cooldown    map[string]time.Time            // account -> rate-limited until
	modelCool   map[string]map[string]time.Time // account -> model -> rate-limited until
	invalid     map[string]bool                 // account -> token rejected upstream (401)
	inFlight    map[string]int64                // account -> upstream requests open right now
	clientLive  map[string]int64                // client -> requests open right now
	tokens      map[string]TokenUsage           // account -> tokens served
	clientTok   map[string]TokenUsage           // client -> tokens used
	apiUsage    map[string]windows              // account -> last usage API utilization
	lastSwitch  map[PinKey]time.Time            // pin -> last proactive move
	switchWhy   map[PinKey]string               // pin -> why it last moved
	policy      SwitchPolicy
}

// SwitchPolicy moves a sticky client off its account before the account's
// limit, as claude-swap's auto-switch does: once the account's binding
// window (the higher of its 5-hour and 7-day utilization) reaches
// Threshold, the client moves to the account with the most headroom,
// provided that one is itself below Threshold and at least Hysteresis
// points lower (so two accounts near the line never ping-pong). A client
// moves proactively at most once per Cooldown; an account that actually
// hits its limit (429) or fails is left at once regardless.
type SwitchPolicy struct {
	Threshold  float64 // percent; 0 disables proactive moves
	Hysteresis float64 // percentage points
	Cooldown   time.Duration
}

// DefaultSwitchPolicy is claude-swap's default: move at 90%.
var DefaultSwitchPolicy = SwitchPolicy{Threshold: 90, Hysteresis: 10, Cooldown: 5 * time.Minute}

// windows is one utilization sample of an account's 5-hour and 7-day
// windows (percent; nil when unknown).
type windows struct {
	fiveHour, sevenDay *float64
	fiveReset          *time.Time
	sevenReset         *time.Time
	at                 time.Time
}

// binding returns the higher window, counting a window whose reset has
// passed as empty.
func (w windows) binding(now time.Time) (float64, bool) {
	pct, known := 0.0, false
	for _, win := range []struct {
		v     *float64
		reset *time.Time
	}{{w.fiveHour, w.fiveReset}, {w.sevenDay, w.sevenReset}} {
		if win.v == nil {
			continue
		}
		known = true
		if win.reset != nil && !now.Before(*win.reset) {
			continue // the window has rolled over since
		}
		pct = max(pct, *win.v)
	}
	return pct, known
}

// SetSwitchPolicy sets how sticky clients move between accounts.
func (s *Selector) SetSwitchPolicy(p SwitchPolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policy = p
}

// MarkUsageAPI records an account's utilization from the usage API, which
// (unlike response headers) also covers accounts not used lately.
func (s *Selector) MarkUsageAPI(name string, fiveHour *float64, fiveReset *time.Time, sevenDay *float64, sevenReset *time.Time, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apiUsage[name] = windows{fiveHour, sevenDay, fiveReset, sevenReset, at}
}

// utilization returns an account's binding-window percentage from the
// fresher of its last response headers and the usage API. The caller must
// hold s.mu.
func (s *Selector) utilization(name string, now time.Time) (float64, bool) {
	var best windows
	if rl := s.rateLimits[name]; rl != nil && (rl.FiveHourPct != nil || rl.SevenDayPct != nil) {
		best = windows{rl.FiveHourPct, rl.SevenDayPct, rl.FiveHourReset, rl.SevenDayReset, rl.Seen}
	}
	if api, ok := s.apiUsage[name]; ok && api.at.After(best.at) {
		best = api
	}
	return best.binding(now)
}

// mostHeadroom picks, among avail except skip, the account with the lowest
// known utilization strictly below limit. Accounts with no utilization data
// are taken only when no known one qualifies. The caller must hold s.mu.
func (s *Selector) mostHeadroom(avail []Account, skip string, limit float64, now time.Time) (Account, float64, bool) {
	var pick Account
	pickPct, found := 0.0, false
	var unknown []Account
	for _, a := range avail {
		if a.Name == skip {
			continue
		}
		pct, known := s.utilization(a.Name, now)
		if !known {
			unknown = append(unknown, a)
			continue
		}
		if pct < limit && (!found || pct < pickPct) {
			pick, pickPct, found = a, pct, true
		}
	}
	if !found && len(unknown) > 0 {
		return unknown[0], 0, true
	}
	return pick, pickPct, found
}

// SwitchReason says why a route's sticky pin last moved.
func (s *Selector) SwitchReason(r Route) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.switchWhy[r.key()]; r != "" {
		return r
	}
	return "previous account unavailable"
}

func first(a Account, avail []Account) []Account {
	i := slices.IndexFunc(avail, func(x Account) bool { return x.Name == a.Name })
	rest := slices.Delete(slices.Clone(avail), i, i+1)
	return append([]Account{a}, rest...)
}

// TokenUsage counts tokens as Anthropic reports them in responses.
type TokenUsage struct {
	Input      int64 `json:"input"` // uncached input
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`  // input served from the prompt cache
	CacheWrite int64 `json:"cache_write"` // input written to the prompt cache
}

// TotalInput is every input token, cached or not.
func (u TokenUsage) TotalInput() int64 { return u.Input + u.CacheRead + u.CacheWrite }

// IsZero reports whether nothing was counted.
func (u TokenUsage) IsZero() bool { return u == TokenUsage{} }

// Add sums two usages.
func (u TokenUsage) Add(o TokenUsage) TokenUsage {
	return TokenUsage{u.Input + o.Input, u.Output + o.Output, u.CacheRead + o.CacheRead, u.CacheWrite + o.CacheWrite}
}

// NewSelector returns an empty selector.
func NewSelector() *Selector {
	return &Selector{
		rr:          map[string]int{},
		served:      map[string]int64{},
		rateLimited: map[string]int64{},
		rejected:    map[string]int64{},
		upstreamErr: map[string]int64{},
		lastUsed:    map[string]time.Time{},
		clientReq:   map[string]int64{},
		clientLast:  map[string]time.Time{},
		rateLimits:  map[string]*RateLimitInfo{},
		sticky:      map[PinKey]string{},
		pinSeen:     map[PinKey]time.Time{},
		cooldown:    map[string]time.Time{},
		modelCool:   map[string]map[string]time.Time{},
		invalid:     map[string]bool{},
		inFlight:    map[string]int64{},
		clientLive:  map[string]int64{},
		tokens:      map[string]TokenUsage{},
		clientTok:   map[string]TokenUsage{},
		apiUsage:    map[string]windows{},
		lastSwitch:  map[PinKey]time.Time{},
		switchWhy:   map[PinKey]string{},
		policy:      DefaultSwitchPolicy,
	}
}

// Ranked returns the pool's usable members ordered best-first for this
// request. Disabled, cooling-down and rejected members are dropped, and so
// are members cooling down for the route's model. The route's session (or,
// without one, its client) keys the sticky strategy.
func (s *Selector) Ranked(members []Account, strategy Strategy, r Route) []Account {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.pruneSessions(now)
	avail := make([]Account, 0, len(members))
	for _, m := range members {
		if m.Disabled || s.invalid[m.Name] {
			continue
		}
		if until, ok := s.cooldown[m.Name]; ok {
			if until.After(now) {
				continue
			}
			delete(s.cooldown, m.Name) // expired
		}
		if until, ok := s.modelCool[m.Name][r.Model]; ok && r.Model != "" {
			if until.After(now) {
				continue
			}
			delete(s.modelCool[m.Name], r.Model)
		}
		avail = append(avail, m)
	}
	poolName := r.Pool
	if len(avail) == 0 {
		return nil
	}

	switch strategy {
	case StrategyRoundRobin:
		return s.rotate(poolName, avail)

	case StrategyLeastUsed:
		least := slices.Clone(avail)
		sort.SliceStable(least, func(i, j int) bool {
			return s.served[least[i].Name] < s.served[least[j].Name]
		})
		return least

	case StrategyRandom:
		sh := slices.Clone(avail)
		rand.Shuffle(len(sh), func(i, j int) { sh[i], sh[j] = sh[j], sh[i] })
		return sh

	default: // StrategySticky
		key := r.key()
		s.pinSeen[key] = now
		p := s.policy
		if want := s.sticky[key]; want != "" {
			if i := slices.IndexFunc(avail, func(a Account) bool { return a.Name == want }); i >= 0 {
				// Move before the limit when the pinned account is nearly
				// spent and a clearly better one exists.
				if cur, known := s.utilization(want, now); p.Threshold > 0 && known && cur >= p.Threshold &&
					now.Sub(s.lastSwitch[key]) >= p.Cooldown {
					if best, pct, ok := s.mostHeadroom(avail, want, min(p.Threshold, cur-p.Hysteresis), now); ok {
						s.sticky[key], s.lastSwitch[key] = best.Name, now
						s.switchWhy[key] = fmt.Sprintf("%s at %.0f%% (switches at %.0f%%), %s at %.0f%%",
							want, cur, p.Threshold, best.Name, pct)
						return first(best, avail)
					}
				}
				return first(avail[i], avail)
			}
			// The pinned account is unavailable; fall through and remap.
		}
		delete(s.switchWhy, key)
		// New pins go to the account with the most headroom when that is
		// known, else round-robin.
		limit := p.Threshold
		if limit <= 0 {
			limit = 101
		}
		if best, _, ok := s.mostHeadroom(avail, "", limit, now); ok && s.anyKnown(avail, now) {
			s.sticky[key] = best.Name
			return first(best, avail)
		}
		order := s.rotate(poolName, avail)
		s.sticky[key] = order[0].Name
		return order
	}
}

// pruneSessions drops session pins idle for longer than SessionTTL, at most
// once a minute. Client pins (no session) are kept. The caller must hold
// s.mu.
func (s *Selector) pruneSessions(now time.Time) {
	if now.Sub(s.lastPrune) < time.Minute {
		return
	}
	s.lastPrune = now
	for key, seen := range s.pinSeen {
		if key.Session != "" && now.Sub(seen) > SessionTTL {
			s.dropPin(key)
		}
	}
}

// dropPin forgets one pin. The caller must hold s.mu.
func (s *Selector) dropPin(key PinKey) {
	delete(s.sticky, key)
	delete(s.pinSeen, key)
	delete(s.lastSwitch, key)
	delete(s.switchWhy, key)
}

// anyKnown reports whether any of avail has utilization data. The caller
// must hold s.mu.
func (s *Selector) anyKnown(avail []Account, now time.Time) bool {
	for _, a := range avail {
		if _, known := s.utilization(a.Name, now); known {
			return true
		}
	}
	return false
}

// rotate advances the pool's round-robin cursor and returns members rotated
// to start at the cursor. The caller must hold s.mu.
func (s *Selector) rotate(poolName string, members []Account) []Account {
	if len(members) < 2 {
		return members
	}
	i := s.rr[poolName] % len(members)
	s.rr[poolName]++
	return append(slices.Clone(members[i:]), members[:i]...)
}

// Assignment returns the account a route is currently pinned to, or "".
func (s *Selector) Assignment(r Route) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sticky[r.key()]
}

// Pin moves a route's sticky assignment to account, e.g. after a failover
// landed its request there.
func (s *Selector) Pin(r Route, account string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sticky[r.key()] = account
	s.pinSeen[r.key()] = time.Now()
}

// MarkSuccess records a completed upstream request. It leaves cooldowns
// alone: a request that was already in flight when its account was
// rate-limited can still succeed, and must not lift the limit.
func (s *Selector) MarkSuccess(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.served[name]++
	s.lastUsed[name] = time.Now()
}

// MarkRateLimited records a 429 response and puts an account on cooldown
// until t (never shortening an existing longer cooldown).
func (s *Selector) MarkRateLimited(name string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimited[name]++
	if cur, ok := s.cooldown[name]; !ok || until.After(cur) {
		s.cooldown[name] = until
	}
}

// MarkModelRateLimited records a 429 that limits only one model on an
// account (a per-model weekly limit, or extra usage running out while the
// subscription windows still have room): the account keeps serving other
// models, and this model is skipped on it until t.
func (s *Selector) MarkModelRateLimited(name, model string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimited[name]++
	m := s.modelCool[name]
	if m == nil {
		m = map[string]time.Time{}
		s.modelCool[name] = m
	}
	if cur, ok := m[model]; !ok || until.After(cur) {
		m[model] = until
	}
}

// MarkForbidden records a 403 for an account that another account then
// served, and puts it on cooldown until t: a disabled organization or a
// revoked grant, which a reload with a new token (ClearAccount) lifts.
func (s *Selector) MarkForbidden(name string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejected[name]++
	if cur, ok := s.cooldown[name]; !ok || until.After(cur) {
		s.cooldown[name] = until
	}
}

// MarkError records a transport error and puts an account on a short cooldown
// (never shortening an existing longer cooldown).
func (s *Selector) MarkError(name string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upstreamErr[name]++
	if cur, ok := s.cooldown[name]; !ok || until.After(cur) {
		s.cooldown[name] = until
	}
}

// MarkInvalid marks an account's token as rejected upstream (401) until the
// credential is fixed and reloaded.
func (s *Selector) MarkInvalid(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalid[name] = true
	s.rejected[name]++
}

// Begin counts an upstream request open on an account until the returned
// func is called (once the response has been streamed to the client or the
// attempt failed). Calling it more than once is harmless.
func (s *Selector) Begin(name string) (end func()) {
	s.mu.Lock()
	s.inFlight[name]++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.inFlight[name]--
			s.mu.Unlock()
		})
	}
}

// BeginClient counts a client request open until the returned func is called.
func (s *Selector) BeginClient(client string) (end func()) {
	s.mu.Lock()
	s.clientLive[client]++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.clientLive[client]--
			s.mu.Unlock()
		})
	}
}

// ForgetClient drops a client's stats and sticky pins (a runtime client
// that went away).
func (s *Selector) ForgetClient(client string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clientReq, client)
	delete(s.clientLast, client)
	delete(s.clientLive, client)
	delete(s.clientTok, client)
	for key := range s.sticky {
		if key.Client == client {
			s.dropPin(key)
		}
	}
}

// AddTokens records the tokens of one response, for its account and client.
func (s *Selector) AddTokens(account, client string, u TokenUsage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[account] = s.tokens[account].Add(u)
	s.clientTok[client] = s.clientTok[client].Add(u)
}

// MarkClientAttempt records that a client request was accepted (after auth
// and pool routing, regardless of upstream outcome).
func (s *Selector) MarkClientAttempt(client string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clientReq[client]++
	s.clientLast[client] = time.Now()
}

// AccountStats is one account's cumulative proxy stats for the usage page.
type AccountStats struct {
	Requests     int64
	RateLimited  int64
	Rejected     int64
	Errors       int64
	LastUsed     time.Time
	CoolingUntil time.Time
	ModelCooling map[string]time.Time // model -> cooling until, for models limited on their own
	Invalid      bool
	InFlight     int64 // upstream requests open right now
	Tokens       TokenUsage
	RateLimits   *RateLimitInfo // last anthropic-ratelimit-* headers seen
}

// ClientStats is one client's cumulative proxy stats for the usage page.
type ClientStats struct {
	Requests int64
	LastSeen time.Time
	Live     int64 // requests open right now
	Tokens   TokenUsage
}

// RateLimitInfo is the last rate-limit snapshot seen in an account's
// anthropic-ratelimit-* response headers. Pointer fields are set only when
// the upstream actually sent that header.
type RateLimitInfo struct {
	Status            string     `json:"status,omitempty"` // unified-status: allowed/allowed_warning/rejected
	RequestsLimit     *int64     `json:"requests_limit,omitempty"`
	RequestsRemaining *int64     `json:"requests_remaining,omitempty"`
	RequestsReset     *time.Time `json:"requests_reset,omitempty"`
	TokensLimit       *int64     `json:"tokens_limit,omitempty"`
	TokensRemaining   *int64     `json:"tokens_remaining,omitempty"`
	TokensReset       *time.Time `json:"tokens_reset,omitempty"`

	// Subscription windows (OAuth accounts): anthropic-ratelimit-unified-*.
	FiveHourPct   *float64   `json:"five_hour_pct,omitempty"` // 0–100
	FiveHourReset *time.Time `json:"five_hour_reset,omitempty"`
	SevenDayPct   *float64   `json:"seven_day_pct,omitempty"`
	SevenDayReset *time.Time `json:"seven_day_reset,omitempty"`
	Claim         string     `json:"claim,omitempty"`          // the window that binds: five_hour, seven_day…
	OverageStatus string     `json:"overage_status,omitempty"` // extra usage: allowed / rejected
	OverageReason string     `json:"overage_reason,omitempty"` // e.g. org_spend_cap_reached

	Seen time.Time `json:"seen"`
}

// MarkRateLimits records the last rate-limit headers seen for an account.
func (s *Selector) MarkRateLimits(name string, info RateLimitInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := info
	s.rateLimits[name] = &cp
}

// StatsSnapshot returns cumulative stats for the named accounts. Accounts
// with no recorded activity are included with zero values.
func (s *Selector) StatsSnapshot(names []string) map[string]AccountStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := make(map[string]AccountStats, len(names))
	for _, n := range names {
		var models map[string]time.Time
		for model, until := range s.modelCool[n] {
			if until.After(now) {
				if models == nil {
					models = map[string]time.Time{}
				}
				models[model] = until
			}
		}
		out[n] = AccountStats{
			Requests:     s.served[n],
			RateLimited:  s.rateLimited[n],
			Rejected:     s.rejected[n],
			Errors:       s.upstreamErr[n],
			LastUsed:     s.lastUsed[n],
			CoolingUntil: s.cooldown[n],
			ModelCooling: models,
			Invalid:      s.invalid[n],
			InFlight:     s.inFlight[n],
			Tokens:       s.tokens[n],
			RateLimits:   s.rateLimits[n],
		}
	}
	return out
}

// ClientStatsSnapshot returns cumulative stats for every client that has made
// at least one request.
func (s *Selector) ClientStatsSnapshot() map[string]ClientStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]ClientStats, len(s.clientReq))
	for c := range s.clientReq {
		out[c] = ClientStats{Requests: s.clientReq[c], LastSeen: s.clientLast[c], Live: s.clientLive[c], Tokens: s.clientTok[c]}
	}
	return out
}

// StickySnapshot returns the current sticky assignments, most recently
// used first.
func (s *Selector) StickySnapshot() []Pin {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Pin, 0, len(s.sticky))
	for k, v := range s.sticky {
		out = append(out, Pin{k, v, s.pinSeen[k]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seen.After(out[j].Seen) })
	return out
}

// ClearAccount forgets an account's health state (used after a reload that
// replaced its token).
func (s *Selector) ClearAccount(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.invalid, name)
	delete(s.cooldown, name)
	delete(s.modelCool, name)
	delete(s.rateLimits, name)
}

// NextRecovery returns the earliest time a currently-cooling member becomes
// usable again for model (empty: any model), or the zero time if none.
func (s *Selector) NextRecovery(members []Account, model string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var best time.Time
	for _, m := range members {
		if m.Disabled || s.invalid[m.Name] {
			continue
		}
		until := s.cooldown[m.Name]
		if mu := s.modelCool[m.Name][model]; model != "" && mu.After(until) {
			until = mu
		}
		if until.After(now) && (best.IsZero() || until.Before(best)) {
			best = until
		}
	}
	return best
}
