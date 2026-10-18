// Real per-account usage from Anthropic's OAuth usage API
// (GET /api/oauth/usage). Account tokens carrying the user:profile scope —
// the default for `cproxy setup-token` — answer it. cproxy polls it lazily
// (started by the first usage view) in the background and PREFERS these
// numbers over the usage inferred from anthropic-ratelimit-* response
// headers, which remains the labeled fallback for tokens that cannot query
// the API (inference-only tokens, API keys).

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dehydr8/cproxy/internal/pool"
	"github.com/dehydr8/cproxy/internal/version"
)

const (
	usageAPIPath     = "/api/oauth/usage"
	usageAPITimeout  = 5 * time.Second  // per account fetch
	usageCycleBudget = 10 * time.Second // per background refresh cycle
	usageTick        = 15 * time.Second // background poll cadence
	usageFreshFor    = 60 * time.Second // success TTL
	usageMinBackoff  = 30 * time.Second
	usageMaxBackoff  = 30 * time.Minute
	usageForbidden   = 30 * time.Minute // 403: token lacks the scope
	usageWarmWait    = 2 * time.Second  // first view waits for first cycle
	usageIdleAfter   = 5 * time.Minute  // stop polling when nobody has looked
)

// UsageAPI is one account's real usage as answered by the usage API. It is
// the preferred source over header-inferred rate limits.
type UsageAPI struct {
	FiveHour  *UsageAPIWindow  `json:"five_hour,omitempty"`
	SevenDay  *UsageAPIWindow  `json:"seven_day,omitempty"`
	Spend     *UsageAPISpend   `json:"spend,omitempty"`
	Scoped    []UsageAPIScoped `json:"scoped,omitempty"`
	FetchedAt time.Time        `json:"fetched_at"`
}

type UsageAPIWindow struct {
	Pct      float64    `json:"pct"`
	ResetsAt *time.Time `json:"resets_at,omitempty"`
}

type UsageAPISpend struct {
	Used     *float64   `json:"used,omitempty"`
	Limit    *float64   `json:"limit,omitempty"`
	Pct      float64    `json:"pct"`
	Currency string     `json:"currency,omitempty"`
	ResetsAt *time.Time `json:"resets_at,omitempty"`
}

type UsageAPIScoped struct {
	Name     string     `json:"name"`
	Pct      float64    `json:"pct"`
	ResetsAt *time.Time `json:"resets_at,omitempty"`
}

// usageAPIResponse is the raw wire shape of /api/oauth/usage.
type usageAPIResponse struct {
	FiveHour   *usageAPIWindow `json:"five_hour"`
	SevenDay   *usageAPIWindow `json:"seven_day"`
	ExtraUsage *usageAPIExtra  `json:"extra_usage"`
	Limits     []usageAPILimit `json:"limits"`
}

type usageAPIWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

type usageAPIExtra struct {
	IsEnabled    bool     `json:"is_enabled"`
	UsedCredits  *float64 `json:"used_credits"`
	MonthlyLimit *float64 `json:"monthly_limit"`
	Utilization  *float64 `json:"utilization"`
	Currency     string   `json:"currency"`
	ResetsAt     string   `json:"resets_at"`
}

type usageAPILimit struct {
	Scope struct {
		Model struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
	Percent  *float64 `json:"percent"`
	ResetsAt string   `json:"resets_at"`
}

// fetchUsageAPI queries one account token against the usage API.
func fetchUsageAPI(ctx context.Context, client *http.Client, upstream *url.URL, token string) (*UsageAPI, error) {
	u := *upstream
	u.Path = strings.TrimSuffix(u.Path, "/") + usageAPIPath
	u.RawQuery = ""
	u.Fragment = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("usage API: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "cproxy/"+version.String()+" (github.com/dehydr8/cproxy)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("usage API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		ra := time.Duration(0)
		if secs, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && secs > 0 {
			ra = time.Duration(secs) * time.Second
		}
		return nil, &usageAPIError{status: resp.StatusCode, retryAfter: ra}
	}
	var raw usageAPIResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("usage API: decoding response: %w", err)
	}
	return raw.usage(time.Now()), nil
}

// usage converts the wire response into the normalized snapshot.
func (raw *usageAPIResponse) usage(now time.Time) *UsageAPI {
	out := &UsageAPI{FetchedAt: now}
	if raw.FiveHour != nil {
		out.FiveHour = &UsageAPIWindow{Pct: deref(raw.FiveHour.Utilization), ResetsAt: parseUsageTime(raw.FiveHour.ResetsAt)}
	}
	if raw.SevenDay != nil {
		out.SevenDay = &UsageAPIWindow{Pct: deref(raw.SevenDay.Utilization), ResetsAt: parseUsageTime(raw.SevenDay.ResetsAt)}
	}
	if raw.ExtraUsage != nil && raw.ExtraUsage.IsEnabled {
		out.Spend = &UsageAPISpend{
			Used:     fromCents(raw.ExtraUsage.UsedCredits),
			Limit:    fromCents(raw.ExtraUsage.MonthlyLimit),
			Pct:      deref(raw.ExtraUsage.Utilization),
			Currency: raw.ExtraUsage.Currency,
			ResetsAt: parseUsageTime(raw.ExtraUsage.ResetsAt),
		}
	}
	for _, l := range raw.Limits {
		if l.Scope.Model.DisplayName == "" {
			continue
		}
		out.Scoped = append(out.Scoped, UsageAPIScoped{
			Name:     l.Scope.Model.DisplayName,
			Pct:      deref(l.Percent),
			ResetsAt: parseUsageTime(l.ResetsAt),
		})
	}
	return out
}

// usageAPIError marks an upstream HTTP failure so the poller can honor
// Retry-After and back off longer on permission errors.
type usageAPIError struct {
	status     int
	retryAfter time.Duration
}

func (e *usageAPIError) Error() string {
	switch e.status {
	case http.StatusUnauthorized:
		return "usage API: 401 (token rejected)"
	case http.StatusForbidden:
		return "usage API: 403 (token needs the user:profile scope)"
	case http.StatusTooManyRequests:
		return "usage API: 429 (rate limited)"
	default:
		return fmt.Sprintf("usage API: %d", e.status)
	}
}

// usagePoller caches the last usage API result per account name.
type usagePoller struct {
	mu      sync.Mutex
	entries map[string]*usageEntry
}

type usageEntry struct {
	data      *UsageAPI
	err       string
	fetchedAt time.Time
	retryAt   time.Time
	backoff   time.Duration
}

func (p *usagePoller) due(name string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entries[name]
	return e == nil || !e.retryAt.After(now)
}

// record stores one fetch result: fresh data on success, or the error (and
// a backoff schedule) on failure. Failed entries drop stale data so the
// dashboard never shows numbers the token can no longer stand behind.
func (p *usagePoller) record(name string, data *UsageAPI, err error, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entries[name]
	if e == nil {
		e = &usageEntry{}
		p.entries[name] = e
	}
	e.fetchedAt = now
	if err == nil {
		e.data, e.err, e.backoff = data, "", 0
		e.retryAt = now.Add(usageFreshFor)
		return
	}
	e.data, e.err = nil, err.Error()
	var wait time.Duration
	if ue, ok := err.(*usageAPIError); ok {
		switch {
		case ue.status == http.StatusForbidden || ue.status == http.StatusUnauthorized:
			wait = usageForbidden
		case ue.retryAfter > 0:
			wait = ue.retryAfter
		default:
			wait = e.nextBackoff()
		}
	} else {
		wait = e.nextBackoff()
	}
	e.retryAt = now.Add(wait)
}

func (e *usageEntry) nextBackoff() time.Duration {
	if e.backoff == 0 {
		e.backoff = usageMinBackoff
	} else {
		e.backoff = min(e.backoff*2, usageMaxBackoff)
	}
	return e.backoff
}

// forget drops an account's cached result and backoff so the next cycle
// fetches it afresh.
func (p *usagePoller) forget(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, name)
}

// snapshot returns the cached result for an account (nil, nil when never
// fetched — e.g. API-key accounts, which are never polled).
func (p *usagePoller) snapshot(name string) (*UsageAPI, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entries[name]
	if e == nil {
		return nil, ""
	}
	return e.data, e.err
}

// pollUsageOnce refreshes every due OAuth account sequentially under ctx.
func (s *Server) pollUsageOnce(ctx context.Context) {
	st := s.state.Load()
	if st == nil || st.Runtime == nil {
		return
	}
	for _, a := range st.Runtime.Accounts {
		if ctx.Err() != nil {
			return
		}
		if a.Type != pool.TokenTypeOAuth || a.Token == "" {
			continue
		}
		now := time.Now()
		if !s.usage.due(a.Name, now) {
			continue
		}
		fctx, cancel := context.WithTimeout(ctx, usageAPITimeout)
		data, err := fetchUsageAPI(fctx, s.client, s.upstream, a.Token)
		cancel()
		s.usage.record(a.Name, data, err, time.Now())
	}
}

// pollUsageLoop keeps usage warm while anyone is watching: one immediate
// refresh, then a tick every 15s (successful entries are re-fetched at most
// once per minute, failures back off). Ticks are skipped once no usage view
// has arrived for usageIdleAfter, so an unwatched server stops querying the
// usage API until the next view.
func (s *Server) pollUsageLoop() {
	ctx, cancel := context.WithTimeout(context.Background(), usageCycleBudget)
	s.pollUsageOnce(ctx)
	cancel()
	close(s.usageWarm)
	t := time.NewTicker(usageTick)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if time.Since(time.Unix(0, s.usageViewed.Load())) > usageIdleAfter {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), usageCycleBudget)
			s.pollUsageOnce(ctx)
			cancel()
		case <-s.usageStop:
			return
		}
	}
}

// ensureUsageWarm lazily starts the poller on the first usage view and
// waits briefly for its first cycle, so the first dashboard or `cproxy
// usage` view already shows live numbers. Later calls return immediately.
func (s *Server) ensureUsageWarm() {
	s.usageViewed.Store(time.Now().UnixNano())
	s.usageOnce.Do(func() { go s.pollUsageLoop() })
	select {
	case <-s.usageWarm:
	case <-time.After(usageWarmWait):
	}
}

// Close stops the background usage poller. Proxying stops with the
// process; Close is for graceful shutdowns and tests.
func (s *Server) Close() {
	s.closeOnce.Do(func() { close(s.usageStop) })
}

// UsageAPISummary renders real usage API numbers for the dashboard and the
// CLI: 5h/7d utilization with resets, spend and per-model limits. When the
// account token cannot query the API, the error note is shown instead.
func UsageAPISummary(u *UsageAPI, errText string, now time.Time) string {
	if u == nil {
		switch {
		case errText != "":
			return errText
		default:
			return "—"
		}
	}
	var parts []string
	if u.FiveHour != nil {
		parts = append(parts, windowPart("5h", u.FiveHour, now))
	}
	if u.SevenDay != nil {
		parts = append(parts, windowPart("7d", u.SevenDay, now))
	}
	if u.Spend != nil {
		parts = append(parts, spendPart(u.Spend))
	}
	for _, sc := range u.Scoped {
		parts = append(parts, fmt.Sprintf("%s %s", sc.Name, pctStr(sc.Pct)))
	}
	if len(parts) == 0 {
		return "no usage reported"
	}
	return strings.Join(parts, " · ")
}

func windowPart(label string, w *UsageAPIWindow, now time.Time) string {
	s := label + " " + pctStr(w.Pct)
	if w.ResetsAt != nil {
		s += " (resets " + countdown(*w.ResetsAt, now) + ")"
	}
	return s
}

func spendPart(sp *UsageAPISpend) string {
	prefix := ""
	if sp.Currency != "" {
		prefix = sp.Currency + " "
	}
	used := "—"
	if sp.Used != nil {
		used = strconv.FormatFloat(*sp.Used, 'f', -1, 64)
	}
	if sp.Limit != nil {
		return "spend " + prefix + used + "/" + strconv.FormatFloat(*sp.Limit, 'f', -1, 64)
	}
	return "spend " + prefix + used + " (no limit)"
}

func pctStr(pct float64) string {
	return strconv.FormatFloat(pct, 'f', -1, 64) + "%"
}

func parseUsageTime(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t
	}
	return nil
}

// fromCents converts extra-usage credit amounts, which the API reports in
// minor currency units (claude-swap divides them by 100 too).
func fromCents(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p / 100
	return &v
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
