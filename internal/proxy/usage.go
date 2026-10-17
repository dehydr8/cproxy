// Usage dashboard: an auth-gated live page at /usage showing pool, account
// and client activity, plus the rate-limit headers cproxy observes on
// upstream responses. It refreshes itself with htmx (partial HTML swap every
// 2s) and falls back to manual reload when htmx cannot load. `?format=json`
// returns the same data machine-readable for scripts and agents.

package proxy

import (
	"encoding/json"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dehydr8/cproxy/internal/config"
	"github.com/dehydr8/cproxy/internal/pool"
	"github.com/dehydr8/cproxy/internal/version"
)

type accountRow struct {
	Name        string
	Type        string
	Pools       string
	Token       string // masked
	Requests    int64
	RateLimited int64
	Rejected    int64
	Errors      int64
	State       string // ok / cooling … / invalid token / disabled
	StateClass  string // CSS class for the state cell
	UsageAPI    string // real usage API numbers, preferred over the headers
	RateLimits  string // last anthropic-ratelimit-* headers, humanized
	LastUsed    string
}

type clientRow struct {
	Name     string
	Pool     string
	Pinned   string
	Requests int64
	LastSeen string
}

type usageData struct {
	Version    string
	Upstream   string
	Accounts   []accountRow
	Clients    []clientRow
	Uptime     string
	TokenParam string // set when the page was opened with ?token=…
}

var usageTmpl = template.Must(template.New("usage").Parse(`
{{define "stats"}}
<div class="meta">
  <b>{{.Version}}</b> &middot; upstream <b>{{.Upstream}}</b> &middot;
  accounts <b>{{len .Accounts}}</b> &middot; clients <b>{{len .Clients}}</b> &middot;
  up <b>{{.Uptime}}</b>
</div>
<table>
  <thead><tr><th>account</th><th>type</th><th>pools</th><th>token</th>
    <th>requests</th><th>429s</th><th>errors</th><th>rejected</th>
    <th>state</th><th>usage</th><th>rate limits</th><th>last used</th></tr></thead>
  <tbody>
  {{range .Accounts}}
    <tr><td>{{.Name}}</td><td>{{.Type}}</td><td>{{.Pools}}</td><td>{{.Token}}</td>
      <td class="num">{{.Requests}}</td><td class="num">{{.RateLimited}}</td>
      <td class="num">{{.Errors}}</td><td class="num">{{.Rejected}}</td>
      <td class="{{.StateClass}}">{{.State}}</td><td class="ua">{{.UsageAPI}}</td>
      <td class="rl">{{.RateLimits}}</td><td>{{.LastUsed}}</td></tr>
  {{else}}
    <tr><td colspan="12">no accounts configured</td></tr>
  {{end}}
  </tbody>
</table>
<table>
  <thead><tr><th>client</th><th>pool</th><th>pinned account</th>
    <th>requests</th><th>last seen</th></tr></thead>
  <tbody>
  {{range .Clients}}
    <tr><td>{{.Name}}</td><td>{{.Pool}}</td><td>{{.Pinned}}</td>
      <td class="num">{{.Requests}}</td><td>{{.LastSeen}}</td></tr>
  {{else}}
    <tr><td colspan="5">no clients yet</td></tr>
  {{end}}
  </tbody>
</table>
{{end}}

{{define "page"}}
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>cproxy usage</title>
<style>
  :root { color-scheme: dark; }
  body { font: 14px/1.55 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
         background: #0e1116; color: #cfd8e3; margin: 2rem auto; max-width: 84rem; padding: 0 1rem; }
  h1 { font-size: 1.15rem; letter-spacing: .12em; margin: 0; color: #e6edf3; }
  h1 span { color: #7fb4e8; }
  .meta { color: #8896a6; font-size: .8rem; margin: .5rem 0 1.75rem; }
  .meta b { color: #cfd8e3; font-weight: 500; }
  table { border-collapse: collapse; margin-bottom: 2rem; width: 100%; }
  th, td { text-align: left; padding: .45rem .8rem; border-bottom: 1px solid #1f2630; white-space: nowrap; }
  td.rl { white-space: normal; min-width: 15rem; max-width: 22rem; }
  td.ua { white-space: normal; min-width: 12rem; max-width: 20rem; }
  th { color: #8896a6; font-size: .72rem; text-transform: uppercase; letter-spacing: .08em; font-weight: 500; }
  td.num { text-align: right; font-variant-numeric: tabular-nums; }
  td.invalid, td.cooling { color: #e6b455; }
  td.disabled { color: #8896a6; }
  td.ok { color: #7fd4a8; }
</style>
<script src="https://unpkg.com/htmx.org@2" defer></script>
</head>
<body>
<h1><span>cproxy</span> usage</h1>
<main id="stats"
      hx-get="/usage?fragment=1{{if .TokenParam}}&amp;token={{.TokenParam}}{{end}}"
      hx-trigger="every 2s" hx-swap="innerHTML">
{{template "stats" .}}
</main>
</body>
</html>
{{end}}
`))

// serveUsage renders the dashboard: the full page, the stats-only fragment
// (?fragment=1, for htmx polling) or machine-readable JSON (?format=json).
func (s *Server) serveUsage(w http.ResponseWriter, r *http.Request, st *State) {
	s.ensureUsageWarm() // start the usage-API poller and let its first cycle land
	if r.URL.Query().Get("format") == "json" {
		s.serveUsageJSON(w, st)
		return
	}

	tokParam := ""
	if tok := strings.TrimSpace(r.URL.Query().Get("token")); tok != "" {
		tokParam = tok
	}
	data := s.usageData(st, tokParam)
	name := "page"
	if r.URL.Query().Get("fragment") == "1" {
		name = "stats"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := usageTmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Warn("usage render failed", "err", err.Error())
	}
}

func (s *Server) usageData(st *State, tokParam string) usageData {
	now := time.Now()
	data := usageData{
		Version:    version.String(),
		Upstream:   s.upstream.String(),
		Uptime:     humanDur(now.Sub(s.started)),
		TokenParam: tokParam,
	}

	var accounts []pool.Account
	clients := map[string]config.ClientAuth{}
	if st.Runtime != nil {
		accounts = st.Runtime.Accounts
		clients = st.Runtime.ClientAuth
	}

	names := make([]string, len(accounts))
	for i, a := range accounts {
		names[i] = a.Name
	}
	stats := s.sel.StatsSnapshot(names)
	sticky := s.sel.StickySnapshot()
	clientStats := s.sel.ClientStatsSnapshot()

	for _, a := range accounts {
		stt := stats[a.Name]
		apiData, apiErr := s.usage.snapshot(a.Name)
		state, class := "ok", "ok"
		switch {
		case a.Disabled:
			state, class = "disabled", "disabled"
		case stt.Invalid:
			state, class = "invalid token", "invalid"
		case stt.CoolingUntil.After(now):
			state, class = "cooling "+humanDur(stt.CoolingUntil.Sub(now)), "cooling"
		}
		data.Accounts = append(data.Accounts, accountRow{
			Name:        a.Name,
			Type:        string(a.Type),
			Pools:       poolsLabel(a.Pools),
			Token:       pool.MaskToken(a.Token),
			Requests:    stt.Requests,
			RateLimited: stt.RateLimited,
			Rejected:    stt.Rejected,
			Errors:      stt.Errors,
			State:       state,
			StateClass:  class,
			UsageAPI:    UsageAPISummary(apiData, apiErr, now),
			RateLimits:  RateLimitsSummary(stt.RateLimits, now),
			LastUsed:    ago(stt.LastUsed),
		})
	}
	slices.SortFunc(data.Accounts, func(a, b accountRow) int { return strings.Compare(a.Name, b.Name) })

	for _, cca := range clients {
		data.Clients = append(data.Clients, clientRow{
			Name:     cca.Name,
			Pool:     poolNameFor(cca),
			Pinned:   pinnedLabel(sticky[poolNameFor(cca)+"|"+cca.Name]),
			Requests: clientStats[cca.Name].Requests,
			LastSeen: ago(clientStats[cca.Name].LastSeen),
		})
	}
	slices.SortFunc(data.Clients, func(a, b clientRow) int { return strings.Compare(a.Name, b.Name) })

	return data
}

// UsageSnapshot is the wire shape of /usage?format=json — the same data as
// the dashboard, for scripts and for `cproxy usage`.
type UsageSnapshot struct {
	Version       string         `json:"version"`
	Upstream      string         `json:"upstream"`
	UptimeSeconds float64        `json:"uptime_seconds"`
	Accounts      []UsageAccount `json:"accounts"`
	Clients       []UsageClient  `json:"clients"`
}

type UsageAccount struct {
	Name         string              `json:"name"`
	Type         string              `json:"type"`
	Pools        []string            `json:"pools"`
	Token        string              `json:"token"` // masked
	Requests     int64               `json:"requests"`
	RateLimited  int64               `json:"rate_limited"`
	Rejected     int64               `json:"rejected"`
	Errors       int64               `json:"errors"`
	State        string              `json:"state"`
	LastUsed     *time.Time          `json:"last_used,omitempty"`
	CoolingUntil *time.Time          `json:"cooling_until,omitempty"`
	UsageAPI     *UsageAPI           `json:"usage_api,omitempty"`       // preferred: real usage API
	UsageAPIErr  string              `json:"usage_api_error,omitempty"` // why the usage API is unavailable
	RateLimits   *pool.RateLimitInfo `json:"rate_limits,omitempty"`     // fallback: inferred from headers
	Invalid      bool                `json:"invalid,omitempty"`
	Disabled     bool                `json:"disabled,omitempty"`
}

type UsageClient struct {
	Name     string     `json:"name"`
	Pool     string     `json:"pool"`
	Pinned   string     `json:"pinned"`
	Requests int64      `json:"requests"`
	LastSeen *time.Time `json:"last_seen,omitempty"`
}

func (s *Server) serveUsageJSON(w http.ResponseWriter, st *State) {
	now := time.Now()
	out := UsageSnapshot{
		Version:       version.String(),
		Upstream:      s.upstream.String(),
		UptimeSeconds: now.Sub(s.started).Seconds(),
		Accounts:      []UsageAccount{},
		Clients:       []UsageClient{},
	}

	var accounts []pool.Account
	clients := map[string]config.ClientAuth{}
	if st.Runtime != nil {
		accounts = st.Runtime.Accounts
		clients = st.Runtime.ClientAuth
	}

	names := make([]string, len(accounts))
	for i, a := range accounts {
		names[i] = a.Name
	}
	stats := s.sel.StatsSnapshot(names)
	sticky := s.sel.StickySnapshot()
	clientStats := s.sel.ClientStatsSnapshot()

	for _, a := range accounts {
		stt := stats[a.Name]
		apiData, apiErr := s.usage.snapshot(a.Name)
		state := "ok"
		switch {
		case a.Disabled:
			state = "disabled"
		case stt.Invalid:
			state = "invalid"
		case stt.CoolingUntil.After(now):
			state = "cooling"
		}
		pools := a.Pools
		if len(pools) == 0 {
			pools = []string{config.DefaultPoolName}
		}
		row := UsageAccount{
			Name:        a.Name,
			Type:        string(a.Type),
			Pools:       pools,
			Token:       pool.MaskToken(a.Token),
			Requests:    stt.Requests,
			RateLimited: stt.RateLimited,
			Rejected:    stt.Rejected,
			Errors:      stt.Errors,
			State:       state,
			UsageAPI:    apiData,
			UsageAPIErr: apiErr,
			RateLimits:  stt.RateLimits,
			Invalid:     stt.Invalid,
			Disabled:    a.Disabled,
		}
		if !stt.LastUsed.IsZero() {
			t := stt.LastUsed
			row.LastUsed = &t
		}
		if stt.CoolingUntil.After(now) {
			t := stt.CoolingUntil
			row.CoolingUntil = &t
		}
		out.Accounts = append(out.Accounts, row)
	}
	slices.SortFunc(out.Accounts, func(a, b UsageAccount) int { return strings.Compare(a.Name, b.Name) })

	for _, cca := range clients {
		row := UsageClient{
			Name:     cca.Name,
			Pool:     poolNameFor(cca),
			Pinned:   pinnedLabel(sticky[poolNameFor(cca)+"|"+cca.Name]),
			Requests: clientStats[cca.Name].Requests,
		}
		if cs, ok := clientStats[cca.Name]; ok && !cs.LastSeen.IsZero() {
			t := cs.LastSeen
			row.LastSeen = &t
		}
		out.Clients = append(out.Clients, row)
	}
	slices.SortFunc(out.Clients, func(a, b UsageClient) int { return strings.Compare(a.Name, b.Name) })

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		s.log.Warn("usage json failed", "err", err.Error())
	}
}

// pinnedLabel renders a client's pinned account, or an em dash when none.
func pinnedLabel(pinned string) string {
	if pinned == "" {
		return "—"
	}
	return pinned
}

// poolsLabel renders an account's pool membership compactly.
func poolsLabel(pools []string) string {
	if len(pools) == 0 {
		return config.DefaultPoolName
	}
	return strings.Join(pools, ",")
}

// RateLimitsSummary renders the last-seen anthropic-ratelimit-* headers.
func RateLimitsSummary(rl *pool.RateLimitInfo, now time.Time) string {
	if rl == nil {
		return "—"
	}
	var parts []string
	if rl.Status != "" && rl.Status != "allowed" {
		parts = append(parts, rl.Status)
	}
	if rl.RequestsRemaining != nil || rl.RequestsLimit != nil {
		parts = append(parts, "req "+compactNum(rl.RequestsRemaining)+"/"+compactNum(rl.RequestsLimit))
	}
	if rl.TokensRemaining != nil || rl.TokensLimit != nil {
		parts = append(parts, "tok "+compactNum(rl.TokensRemaining)+"/"+compactNum(rl.TokensLimit))
	}
	var resets []string
	if rl.RequestsReset != nil {
		resets = append(resets, "req "+countdown(*rl.RequestsReset, now))
	}
	if rl.TokensReset != nil {
		resets = append(resets, "tok "+countdown(*rl.TokensReset, now))
	}
	if len(resets) > 0 {
		parts = append(parts, "reset "+strings.Join(resets, "/"))
	}
	if len(parts) == 0 {
		return "allowed"
	}
	return strings.Join(parts, " · ")
}

func compactNum(v *int64) string {
	if v == nil {
		return "?"
	}
	n := *v
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10)
	case n < 1_000_000:
		return strconv.FormatFloat(float64(n)/1000, 'f', 1, 64) + "k"
	default:
		return strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64) + "M"
	}
}

func countdown(t, now time.Time) string {
	d := t.Sub(now)
	if d <= 0 {
		return "now"
	}
	return humanDur(d)
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return humanDur(time.Since(t).Round(time.Second)) + " ago"
}

func humanDur(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return d.Round(time.Minute).String()
	default:
		return d.Round(time.Hour).String()
	}
}
