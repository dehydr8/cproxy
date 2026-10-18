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
	// StrategySticky pins each client (a run-mode session or a serve-mode
	// client token) to one account and only moves it when that account is
	// rate-limited or rejected. It is the default: a conversation that stays
	// on one account pays its full context there exactly once, instead of
	// every pool member paying for the same context.
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

// Selector tracks sticky assignments, request counters and per-account health
// for the pools served by one proxy process. All methods are safe for
// concurrent use; health state survives serve-mode config reloads.
type Selector struct {
	mu          sync.Mutex
	rr          map[string]int       // pool -> round-robin cursor for new assignments
	served      map[string]int64     // account -> requests served
	rateLimited map[string]int64     // account -> 429 responses seen
	rejected    map[string]int64     // account -> 401 responses seen
	upstreamErr map[string]int64     // account -> transport errors seen
	lastUsed    map[string]time.Time // account -> last request served
	clientReq   map[string]int64     // client -> accepted requests
	clientLast  map[string]time.Time // client -> last accepted request
	rateLimits  map[string]*RateLimitInfo
	sticky      map[string]string    // pool|client -> account name
	cooldown    map[string]time.Time // account -> rate-limited until
	invalid     map[string]bool      // account -> token rejected upstream (401)
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
		sticky:      map[string]string{},
		cooldown:    map[string]time.Time{},
		invalid:     map[string]bool{},
	}
}

// Ranked returns the pool's usable members ordered best-first for this
// request. Disabled, cooling-down and rejected members are dropped.
// clientID keys the sticky strategy; new clients are assigned round-robin.
func (s *Selector) Ranked(members []Account, strategy Strategy, poolName, clientID string) []Account {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
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
		avail = append(avail, m)
	}
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
		key := poolName + "|" + clientID
		if want := s.sticky[key]; want != "" {
			if i := slices.IndexFunc(avail, func(a Account) bool { return a.Name == want }); i >= 0 {
				rest := slices.Delete(slices.Clone(avail), i, i+1)
				return append([]Account{avail[i]}, rest...)
			}
			// The pinned account is unavailable; fall through and remap.
		}
		order := s.rotate(poolName, avail)
		s.sticky[key] = order[0].Name
		return order
	}
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

// Assignment returns the account a client is currently pinned to, or "".
func (s *Selector) Assignment(poolName, clientID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sticky[poolName+"|"+clientID]
}

// Pin moves a client's sticky assignment to account, e.g. after a failover
// landed its request there.
func (s *Selector) Pin(poolName, clientID, account string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sticky[poolName+"|"+clientID] = account
}

// MarkSuccess records a completed upstream request.
func (s *Selector) MarkSuccess(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.served[name]++
	s.lastUsed[name] = time.Now()
	delete(s.cooldown, name)
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
	Invalid      bool
	RateLimits   *RateLimitInfo // last anthropic-ratelimit-* headers seen
}

// ClientStats is one client's cumulative proxy stats for the usage page.
type ClientStats struct {
	Requests int64
	LastSeen time.Time
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
	Seen              time.Time  `json:"seen"`
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
	out := make(map[string]AccountStats, len(names))
	for _, n := range names {
		out[n] = AccountStats{
			Requests:     s.served[n],
			RateLimited:  s.rateLimited[n],
			Rejected:     s.rejected[n],
			Errors:       s.upstreamErr[n],
			LastUsed:     s.lastUsed[n],
			CoolingUntil: s.cooldown[n],
			Invalid:      s.invalid[n],
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
		out[c] = ClientStats{Requests: s.clientReq[c], LastSeen: s.clientLast[c]}
	}
	return out
}

// StickySnapshot returns a copy of the current sticky assignments
// (pool|client -> account name).
func (s *Selector) StickySnapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.sticky))
	for k, v := range s.sticky {
		out[k] = v
	}
	return out
}

// ClearAccount forgets an account's health state (used after a reload that
// replaced its token).
func (s *Selector) ClearAccount(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.invalid, name)
	delete(s.cooldown, name)
	delete(s.rateLimits, name)
}

// NextRecovery returns the earliest time a currently-cooling member becomes
// usable again, or the zero time if none.
func (s *Selector) NextRecovery(members []Account) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var best time.Time
	for _, m := range members {
		if until, ok := s.cooldown[m.Name]; ok && until.After(now) {
			if best.IsZero() || until.Before(best) {
				best = until
			}
		}
	}
	return best
}
