// Usage dashboard: an auth-gated live page at /usage showing pool, account
// and client activity, plus the rate-limit headers cproxy observes on
// upstream responses. A small inline script swaps in the stats fragment
// every 2s; without JavaScript the page is a static snapshot. `?format=json`
// returns the same data machine-readable for scripts and agents.

package proxy

import (
	_ "embed"
	"encoding/json"
	"hash/fnv"
	"html/template"
	"math"
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
	Email       string // from the profile API when the token may read it
	Org         string
	Initials    string
	Hue         int
	Type        string
	Pools       []string
	Token       string // masked
	Requests    int64
	RateLimited int64
	Rejected    int64
	Errors      int64
	State       string // available / cooling 4m / invalid token / disabled
	Tone        string // badge tone: ok, warn, danger or "" (neutral)
	Meters      []meter
	Spend       string
	UsageNote   string // why there are no usage API numbers
	HeaderNote  string // fallback: last anthropic-ratelimit-* headers
	LastUsed    string
	LastUsedAt  string // RFC3339, for the tooltip
}

// meter is one utilization bar (5h, 7d, a per-model limit).
type meter struct {
	Label string
	Text  string // "42.5%"
	Width string // CSS width, clamped to 0–100%
	Tone  string // ok, warn, danger
	Reset string // "resets in 1h30m"
}

type clientRow struct {
	Name     string
	Initials string
	Hue      int
	Pool     string
	Pinned   string
	Requests int64
	LastSeen string
	Active   bool // seen within the last two minutes
}

type usageData struct {
	Version    string
	Upstream   string
	Uptime     string
	TokenParam string // set when the page was opened with ?token=…

	Accounts []accountRow
	Clients  []clientRow

	Available, Cooling, Unusable  int
	Requests, RateLimited, Errors int64
	ActiveClients                 int
}

// usageCSS styles the dashboard; it is inlined so the page needs no other
// request.
//
//go:embed usage.css
var usageCSS string

var usageTmpl = template.Must(template.New("usage").Parse(`
{{define "stats"}}
<div class="tiles">
  <div class="card tile"><div class="label">Accounts available</div>
    <div class="value{{if and .Accounts (eq .Available 0)}} value-danger{{else if .Available}} value-ok{{end}}">{{.Available}}<span class="of">/{{len .Accounts}}</span></div>
    <div class="hint">{{if .Cooling}}{{.Cooling}} cooling down{{else if .Unusable}}{{.Unusable}} unusable{{else}}none cooling down{{end}}</div></div>
  <div class="card tile"><div class="label">Requests served</div>
    <div class="value">{{.Requests}}</div><div class="hint">since start</div></div>
  <div class="card tile"><div class="label">Rate limited</div>
    <div class="value{{if .RateLimited}} value-warn{{end}}">{{.RateLimited}}</div><div class="hint">429s failed over</div></div>
  <div class="card tile"><div class="label">Upstream errors</div>
    <div class="value{{if .Errors}} value-warn{{end}}">{{.Errors}}</div><div class="hint">transport failures</div></div>
  <div class="card tile"><div class="label">Clients</div>
    <div class="value">{{.ActiveClients}}<span class="of">/{{len .Clients}}</span></div><div class="hint">active in the last 2m</div></div>
</div>

<section class="section">
  <div class="table-toolbar"><h2>Accounts<span class="count">{{len .Accounts}}</span></h2></div>
  <div class="table-wrap">
  {{if .Accounts}}
  <table>
    <thead><tr><th scope="col">Account</th><th scope="col">State</th><th scope="col" class="fill">Usage</th>
      <th scope="col" class="num">Requests</th><th scope="col" class="num">429s</th><th scope="col" class="num">Errors</th>
      <th scope="col">Last used</th><th scope="col">Token</th></tr></thead>
    <tbody>
    {{range .Accounts}}
    <tr data-account="{{.Name}}">
      <td><span class="identity"><span class="avatar hue-{{.Hue}}" aria-hidden="true">{{.Initials}}</span>
        <span class="identity-copy"><span class="identity-name">{{.Name}}{{range .Pools}} <span class="chip chip-tag">{{.}}</span>{{end}}</span>
        <span class="identity-sub">{{if .Email}}{{.Email}}{{else}}{{.Type}} token{{end}}</span></span></span></td>
      <td><span class="badge{{if .Tone}} {{.Tone}}{{end}}">{{.State}}</span></td>
      <td class="fill usage">
        {{if .Meters}}<div class="meters">{{range .Meters}}
          <div class="meter"><div class="meter-top"><span>{{.Label}}</span><span class="meter-value {{.Tone}}">{{.Text}}</span></div>
          <div class="meter-track"><div class="meter-fill {{.Tone}}" style="width: {{.Width}}"></div></div>
          {{if .Reset}}<div class="meter-reset">{{.Reset}}</div>{{end}}</div>{{end}}
        </div>{{end}}
        {{if .Spend}}<div class="note">{{.Spend}}</div>{{end}}
        {{if .UsageNote}}<div class="note">{{.UsageNote}}</div>{{end}}
        {{if .HeaderNote}}<div class="note">from response headers: {{.HeaderNote}}</div>{{end}}
        {{if not (or .Meters .Spend .UsageNote .HeaderNote)}}<span class="faint">no usage seen yet</span>{{end}}
      </td>
      <td class="num">{{.Requests}}</td>
      <td class="num{{if .RateLimited}} warn-fg{{end}}">{{.RateLimited}}</td>
      <td class="num{{if .Errors}} warn-fg{{end}}">{{.Errors}}</td>
      <td class="nowrap"{{if .LastUsedAt}} title="{{.LastUsedAt}}"{{end}}>{{.LastUsed}}</td>
      <td><code>{{.Token}}</code></td>
    </tr>
    {{end}}
    </tbody>
  </table>
  {{else}}<p class="empty">No accounts configured. Add one with <code>cproxy accounts add NAME</code> or <code>CPROXY_TOKEN_NAME</code>.</p>{{end}}
  </div>
</section>

<section class="section">
  <div class="table-toolbar"><h2>Clients<span class="count">{{len .Clients}}</span></h2></div>
  <div class="table-wrap">
  {{if .Clients}}
  <table>
    <thead><tr><th scope="col" class="fill">Client</th><th scope="col">Pool</th><th scope="col">Pinned account</th>
      <th scope="col" class="num">Requests</th><th scope="col">Last seen</th></tr></thead>
    <tbody>
    {{range .Clients}}
    <tr data-client="{{.Name}}">
      <td class="fill"><span class="identity identity-compact"><span class="avatar hue-{{.Hue}}" aria-hidden="true">{{.Initials}}</span><span class="identity-name truncate">{{.Name}}</span></span></td>
      <td><span class="chip chip-tag">{{.Pool}}</span></td>
      <td>{{if .Pinned}}<code>{{.Pinned}}</code>{{else}}<span class="faint">–</span>{{end}}</td>
      <td class="num">{{.Requests}}</td>
      <td>{{if .Active}}<span class="presence presence-on">active</span>{{else}}<span class="presence">{{.LastSeen}}</span>{{end}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
  {{else}}<p class="empty">No clients yet.</p>{{end}}
  </div>
</section>
{{end}}

{{define "page"}}<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="theme-color" content="#ffffff" media="(prefers-color-scheme: light)">
<meta name="theme-color" content="#171717" media="(prefers-color-scheme: dark)">
<title>Usage · cproxy</title>
<style>` + usageCSS + `</style>
</head>
<body>
<main>
  <section class="page-head">
    <div class="lede">
      <h1><span class="brand">cproxy</span> usage</h1>
      <p><code>{{.Version}}</code> · upstream <code>{{.Upstream}}</code> · up <span id="uptime">{{.Uptime}}</span></p>
    </div>
    <span id="live" class="presence presence-on" title="refreshes every 2s">live</span>
  </section>
  <div id="stats" data-src="/usage?fragment=1{{if .TokenParam}}&amp;token={{.TokenParam}}{{end}}">
{{template "stats" .}}
  </div>
</main>
<script>
// Swap in the stats fragment every 2s. Inline on purpose: this page may
// carry its client token in the URL, so it loads no third-party script.
(function () {
  var el = document.getElementById("stats"), live = document.getElementById("live");
  function state(ok) {
    live.className = ok ? "presence presence-on" : "presence presence-off";
    live.textContent = ok ? "live" : "reconnecting";
  }
  setInterval(function () {
    fetch(el.dataset.src, {cache: "no-store"})
      .then(function (r) { return r.ok ? r.text() : Promise.reject(r.status); })
      .then(function (html) { el.innerHTML = html; state(true); })
      .catch(function () { state(false); });
  }, 2000);
})();
</script>
</body>
</html>
{{end}}
`))

// serveUsage renders the dashboard: the full page, the stats-only fragment
// (?fragment=1, for the live refresh) or machine-readable JSON (?format=json).
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
	w.Header().Set("Referrer-Policy", "no-referrer") // ?token= must not leak
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

	rt := st.Runtime
	if rt == nil {
		rt = &config.Runtime{DefaultPool: config.DefaultPoolName}
	}
	names := make([]string, len(rt.Accounts))
	for i, a := range rt.Accounts {
		names[i] = a.Name
	}
	stats := s.sel.StatsSnapshot(names)
	sticky := s.sel.StickySnapshot()
	clientStats := s.sel.ClientStatsSnapshot()

	for _, a := range rt.Accounts {
		stt := stats[a.Name]
		apiData, apiErr := s.usage.snapshot(a.Name)
		row := accountRow{
			Name:        a.Name,
			Type:        string(a.Type),
			Pools:       rt.AccountPools(a),
			Token:       pool.MaskToken(a.Token),
			Requests:    stt.Requests,
			RateLimited: stt.RateLimited,
			Rejected:    stt.Rejected,
			Errors:      stt.Errors,
			LastUsed:    ago(stt.LastUsed),
		}
		if !stt.LastUsed.IsZero() {
			row.LastUsedAt = stt.LastUsed.Format(time.RFC3339)
		}
		if prof := s.usage.profile(a.Name); prof != nil {
			row.Email, row.Org = prof.Email, prof.Organization
		}
		row.Initials, row.Hue = avatarFor(row.Name, row.Email)
		switch {
		case a.Disabled:
			row.State = "disabled"
			data.Unusable++
		case stt.Invalid:
			row.State, row.Tone = "invalid token", "danger"
			data.Unusable++
		case stt.CoolingUntil.After(now):
			row.State, row.Tone = "cooling "+humanDur(stt.CoolingUntil.Sub(now)), "warn"
			data.Cooling++
		default:
			row.State, row.Tone = "available", "ok"
			data.Available++
		}
		if apiData != nil {
			if w := apiData.FiveHour; w != nil {
				row.Meters = append(row.Meters, newMeter("5-hour", w.Pct, w.ResetsAt, now))
			}
			if w := apiData.SevenDay; w != nil {
				row.Meters = append(row.Meters, newMeter("7-day", w.Pct, w.ResetsAt, now))
			}
			for _, sc := range apiData.Scoped {
				row.Meters = append(row.Meters, newMeter(sc.Name, sc.Pct, sc.ResetsAt, now))
			}
			if sp := apiData.Spend; sp != nil {
				if sp.Limit != nil {
					m := newMeter("Extra usage", sp.Pct, sp.ResetsAt, now)
					m.Text = money(sp.Currency, sp.Used) + " / " + money(sp.Currency, sp.Limit)
					row.Meters = append(row.Meters, m)
				} else {
					row.Spend = "Extra usage: " + money(sp.Currency, sp.Used) + " spent, no monthly limit"
				}
			}
		} else {
			row.UsageNote = apiErr
			if stt.RateLimits != nil {
				row.HeaderNote = RateLimitsSummary(stt.RateLimits, now)
			}
		}
		data.Requests += stt.Requests
		data.RateLimited += stt.RateLimited
		data.Errors += stt.Errors
		data.Accounts = append(data.Accounts, row)
	}
	slices.SortFunc(data.Accounts, func(a, b accountRow) int { return strings.Compare(a.Name, b.Name) })

	// Configured clients, plus identities seen without one (run mode's
	// "local" session, --no-auth's "anonymous").
	seen := map[string]bool{}
	addClient := func(ca config.ClientAuth) {
		if seen[ca.Name] {
			return
		}
		seen[ca.Name] = true
		cs := clientStats[ca.Name]
		pn := rt.PoolFor(ca)
		row := clientRow{
			Name:     ca.Name,
			Pool:     pn,
			Pinned:   sticky[pn+"|"+ca.Name],
			Requests: cs.Requests,
			LastSeen: ago(cs.LastSeen),
			Active:   !cs.LastSeen.IsZero() && now.Sub(cs.LastSeen) < 2*time.Minute,
		}
		row.Initials, row.Hue = avatarFor(ca.Name, "")
		if row.Active {
			data.ActiveClients++
		}
		data.Clients = append(data.Clients, row)
	}
	for _, ca := range rt.ClientAuth {
		addClient(ca)
	}
	for name := range clientStats {
		addClient(config.ClientAuth{Name: name})
	}
	slices.SortFunc(data.Clients, func(a, b clientRow) int { return strings.Compare(a.Name, b.Name) })

	return data
}

// newMeter builds a utilization bar: green below 70%, amber below 90%, red
// from there.
func newMeter(label string, pct float64, resets *time.Time, now time.Time) meter {
	m := meter{Label: label, Text: pctStr(pct), Tone: "ok"}
	switch {
	case pct >= 90:
		m.Tone = "danger"
	case pct >= 70:
		m.Tone = "warn"
	}
	m.Width = strconv.FormatFloat(math.Max(0, math.Min(100, pct)), 'f', 1, 64) + "%"
	if resets != nil {
		m.Reset = "resets in " + countdown(*resets, now)
	}
	return m
}

// money renders an amount with its currency ($12.40, EUR 3.10).
func money(currency string, v *float64) string {
	if v == nil {
		return "–"
	}
	n := strconv.FormatFloat(*v, 'f', 2, 64)
	switch currency {
	case "", "USD":
		return "$" + n
	default:
		return currency + " " + n
	}
}

// avatarFor derives an avatar's initials and hue (0–5) from an email when
// known, else from a name.
func avatarFor(name, email string) (string, int) {
	src := name
	if email != "" {
		src, _, _ = strings.Cut(email, "@")
	}
	var parts []string
	for _, f := range strings.FieldsFunc(src, func(r rune) bool { return r == '.' || r == '-' || r == '_' || r == ' ' }) {
		parts = append(parts, f)
	}
	ini := ""
	switch {
	case len(parts) >= 2:
		ini = firstRune(parts[0]) + firstRune(parts[1])
	case len(parts) == 1:
		ini = firstRune(parts[0])
		if r := []rune(parts[0]); len(r) > 1 {
			ini += string(r[1])
		}
	}
	h := fnv.New32a()
	h.Write([]byte(email + "|" + name))
	return strings.ToUpper(ini), int(h.Sum32() % 6)
}

func firstRune(s string) string {
	for _, r := range s {
		return string(r)
	}
	return ""
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
	Email        string              `json:"email,omitempty"`        // from the profile API
	Organization string              `json:"organization,omitempty"` // from the profile API
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
		if prof := s.usage.profile(a.Name); prof != nil {
			row.Email, row.Organization = prof.Email, prof.Organization
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
		pn := config.DefaultPoolName
		if st.Runtime != nil {
			pn = st.Runtime.PoolFor(cca)
		}
		row := UsageClient{
			Name:     cca.Name,
			Pool:     pn,
			Pinned:   pinnedLabel(sticky[pn+"|"+cca.Name]),
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

func humanDur(d time.Duration) string { return HumanDuration(d) }

// HumanDuration renders a duration the way people say it: 26s, 47m,
// 1h 32m, 3d 4h. Two units at most; the smaller one is dropped when zero.
func HumanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	two := func(a int64, au string, b int64, bu string) string {
		if b == 0 {
			return strconv.FormatInt(a, 10) + au
		}
		return strconv.FormatInt(a, 10) + au + " " + strconv.FormatInt(b, 10) + bu
	}
	s := int64(d.Round(time.Second) / time.Second)
	if s < 60 {
		return strconv.FormatInt(s, 10) + "s"
	}
	// Round to the smallest unit shown, so 4m59s reads 5m, not 4m.
	if m := (s + 30) / 60; m < 24*60 {
		if m < 60 {
			return strconv.FormatInt(m, 10) + "m"
		}
		return two(m/60, "h", m%60, "m")
	}
	h := (s + 1800) / 3600
	return two(h/24, "d", h%24, "h")
}
