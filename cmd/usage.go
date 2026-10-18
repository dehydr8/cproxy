package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/dehydr8/cproxy/internal/config"
	"github.com/dehydr8/cproxy/internal/daemon"
	"github.com/dehydr8/cproxy/internal/proxy"
	"github.com/dehydr8/cproxy/internal/wrap"
)

var (
	usagePortFlag int
	usageRawJSON  bool
)

var usageCmd = &cobra.Command{
	Use:   "usage [--port N] [--json]",
	Short: "Show live usage of a running cproxy",
	Long: `Print the live usage of the local cproxy daemon in the terminal: pool
accounts with request counts, tokens, rate-limit state and real usage
numbers from Anthropic's usage API, and clients with their sticky pins.
--json prints the raw dashboard JSON instead.

The local daemon (started by the claude wrapper or ` + "`cproxy run`" + `) is asked over
its private socket, so no port or token is needed. With --port, or when no
daemon runs, a ` + "`cproxy serve`" + ` on 127.0.0.1:<port> is asked instead, with a
usage-capable client token picked from the usual places ($CPROXY_AUTH_TOKEN,
a matching $ANTHROPIC_AUTH_TOKEN, $CPROXY_CLIENT_TOKEN_*/CPROXY_CLIENT_TOKENS,
then credentials.json); make one with ` + "`cproxy token --scopes usage`" + `.`,
	RunE: runUsageCmd,
}

func init() {
	usageCmd.Flags().IntVar(&usagePortFlag, "port", defaultPort(),
		"ask a cproxy serve on this port instead of the local daemon")
	usageCmd.Flags().BoolVar(&usageRawJSON, "json", false,
		"print the raw /usage JSON instead of a table")
}

func runUsageCmd(cmd *cobra.Command, args []string) error {
	// The local daemon answers over its private socket: no port, no token.
	if !cmd.Flags().Changed("port") {
		if c, st, err := daemon.Dial(); err == nil {
			body, err := c.Usage()
			if err != nil {
				return err
			}
			return showUsage(body, "daemon "+st.Addr)
		}
	}

	file, err := config.Load()
	if err != nil {
		return err
	}
	rt, err := config.Build(file, os.Environ())
	if err != nil {
		return err
	}
	token, _, err := pickUsageToken(rt, os.Environ())
	if err != nil {
		return err
	}

	addr := fmt.Sprintf("127.0.0.1:%d", usagePortFlag)
	if err := wrap.Verify(addr, token); err != nil {
		if !listening(addr) {
			return fmt.Errorf("no cproxy daemon is running and nothing answers on %s (launch claude through the wrapper or `cproxy run`, or point --port at a `cproxy serve`): %w", addr, err)
		}
		return err
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/usage?format=json", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("no cproxy is answering on %s (is `cproxy serve` or the claude wrapper running? start one with `cproxy serve` or launch claude through the wrapper): %w", addr, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cproxy on %s answered %s: %s", addr, resp.Status, errorBody(body))
	}
	return showUsage(body, addr)
}

// showUsage prints a /usage?format=json body raw (--json) or as tables.
func showUsage(body []byte, where string) error {
	if usageRawJSON {
		os.Stdout.Write(body)
		return nil
	}
	var snap proxy.UsageSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return err
	}
	color := term.IsTerminal(int(os.Stdout.Fd())) && os.Getenv("NO_COLOR") == ""
	return renderUsage(os.Stdout, snap, where, time.Now(), color)
}

// pickUsageToken picks the client token `cproxy usage` authenticates with:
// the same order as the claude wrapper, but preferring tokens that may view
// usage. Unlike the wrapper's pick, a $CPROXY_AUTH_TOKEN that cannot view
// usage does not fail outright — usage is read-only, so the next candidate
// is tried instead and only mentioned if nothing works.
func pickUsageToken(rt *config.Runtime, environ []string) (string, config.ClientAuth, error) {
	capable := func(ca config.ClientAuth) bool { return ca.HasScope(config.ScopeUsage) }

	if t := config.EnvAuthToken(environ); t != "" {
		if ca, ok := rt.ClientAuth[t]; ok && capable(ca) {
			return t, ca, nil
		}
	}
	if t := envValue(environ, "ANTHROPIC_AUTH_TOKEN"); t != "" {
		if ca, ok := rt.ClientAuth[t]; ok && capable(ca) {
			return t, ca, nil
		}
	}
	for _, e := range config.EnvClientTokenList(environ) {
		if ca, ok := rt.ClientAuth[e.Spec.Token]; ok && capable(ca) {
			return e.Spec.Token, ca, nil
		}
	}
	var names []string
	byName := map[string]string{}
	for tok, ca := range rt.ClientAuth {
		if capable(ca) {
			names = append(names, ca.Name)
			byName[ca.Name] = tok
		}
	}
	if len(names) > 0 {
		sort.Strings(names)
		tok := byName[names[0]]
		return tok, rt.ClientAuth[tok], nil
	}

	hint := "generate a usage-capable one with `cproxy token --scopes usage` and add it (CPROXY_CLIENT_TOKENS or client_tokens in credentials.json)"
	if len(rt.ClientAuth) > 0 {
		return "", config.ClientAuth{}, fmt.Errorf("no configured client token may view usage; %s", hint)
	}
	return "", config.ClientAuth{}, fmt.Errorf("no client token is configured; %s", hint)
}

// errorBody extracts the message from a cproxy error response.
func errorBody(body []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := ""
	if json.Unmarshal(body, &env) == nil {
		msg = strings.TrimSpace(env.Error.Message)
	}
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

// renderUsage prints a snapshot cswap-style: a header line, one block per
// account with bar meters for its windows, then the clients. color adds
// ANSI colors (meters green/amber/red, states) for terminals.
func renderUsage(out io.Writer, snap proxy.UsageSnapshot, where string, now time.Time, color bool) error {
	p := palette{on: color}
	var b strings.Builder
	live := int64(0)
	for _, a := range snap.Accounts {
		live += a.InFlight
	}
	head := fmt.Sprintf("cproxy %s · %s · up %s", snap.Version, where,
		humanDur(time.Duration(snap.UptimeSeconds*float64(time.Second))))
	if live > 0 {
		head += " · " + p.green(fmt.Sprintf("%d streaming", live))
	}
	fmt.Fprintln(&b, p.bold(head))

	for _, a := range snap.Accounts {
		b.WriteString("\n")
		id := p.bold(a.Name)
		if a.Plan != "" {
			id += " " + p.blue(a.Plan)
		}
		if a.Email != "" {
			id += "  " + p.dim(a.Email)
		}
		fmt.Fprintln(&b, id)

		state := p.state(a, now)
		if len(a.PinnedBy) > 0 {
			state += p.dim(" · for " + strings.Join(a.PinnedBy, ", "))
		}
		fmt.Fprintln(&b, "  "+state)

		meters := accountMeters(a, now)
		for _, m := range meters {
			fmt.Fprintf(&b, "  %-11s %s %s  %s\n", trunc(m.label, 11), p.bar(m.pct), p.tone(m.pct, fmt.Sprintf("%-6s", m.value)), p.dim(m.note))
		}
		if len(meters) == 0 {
			switch {
			case a.UsageAPIErr != "":
				fmt.Fprintln(&b, "  "+p.dim(a.UsageAPIErr))
			case a.RateLimits != nil:
				fmt.Fprintln(&b, "  "+p.dim("headers: "+proxy.RateLimitsSummary(a.RateLimits, now)))
			default:
				fmt.Fprintln(&b, "  "+p.dim("no usage seen yet"))
			}
		}

		stats := fmt.Sprintf("%d requests", a.Requests)
		if tokens, _ := proxy.TokensLabel(a.Tokens); tokens != "" {
			stats += " · " + tokens
		}
		if a.RateLimited > 0 {
			stats += " · " + p.amber(fmt.Sprintf("%d 429s", a.RateLimited))
		}
		if a.Errors > 0 {
			stats += " · " + p.amber(fmt.Sprintf("%d errors", a.Errors))
		}
		stats += " · last used " + agoString(a.LastUsed, now)
		fmt.Fprintln(&b, "  "+p.dim(stats))
	}
	if len(snap.Accounts) == 0 {
		fmt.Fprintln(&b, "\nno accounts configured")
	}

	if len(snap.Clients) > 0 {
		fmt.Fprintln(&b, "\n"+p.bold("clients"))
		w := tabwriter.NewWriter(&b, 2, 4, 2, ' ', 0)
		for _, c := range snap.Clients {
			seen := agoString(c.LastSeen, now)
			if c.Live > 0 {
				seen = p.green("streaming")
			}
			pin := c.Pool
			if c.Pinned != "" && c.Pinned != "—" {
				pin += " → " + c.Pinned
			}
			tokens, _ := proxy.TokensLabel(c.Tokens)
			fmt.Fprintf(w, "  %s\t%s\t%d req\t%s\t%s\n", c.Name, pin, c.Requests, orDash(tokens), seen)
		}
		w.Flush()
	}
	_, err := io.WriteString(out, b.String())
	return err
}

type meterLine struct {
	label, value, note string
	pct                float64
}

// accountMeters lists an account's windows: from the usage API when
// available, else the subscription windows from its last response headers.
func accountMeters(a proxy.UsageAccount, now time.Time) []meterLine {
	var out []meterLine
	reset := func(t *time.Time) string {
		switch {
		case t == nil:
			return ""
		case !now.Before(*t):
			return "window reset"
		}
		return "resets in " + countdown(*t, now)
	}
	// A reading from before its window rolled over: show the window empty.
	level := func(v float64, t *time.Time) float64 {
		if t != nil && !now.Before(*t) {
			return 0
		}
		return v
	}
	if u := a.UsageAPI; u != nil {
		note := func(s string) string { return s }
		if u.Source == "probe" {
			note = func(s string) string {
				return strings.TrimSpace(s + " (Haiku probe " + agoString(&u.FetchedAt, now) + ")")
			}
		}
		if w := u.FiveHour; w != nil {
			v := level(w.Pct, w.ResetsAt)
			out = append(out, meterLine{"5-hour", pct(v), note(reset(w.ResetsAt)), v})
		}
		if w := u.SevenDay; w != nil {
			v := level(w.Pct, w.ResetsAt)
			out = append(out, meterLine{"7-day", pct(v), note(reset(w.ResetsAt)), v})
		}
		for _, sc := range u.Scoped {
			out = append(out, meterLine{sc.Name, pct(sc.Pct), reset(sc.ResetsAt), sc.Pct})
		}
		if sp := u.Spend; sp != nil && sp.Limit != nil && sp.Used != nil {
			out = append(out, meterLine{"Extra usage", pct(sp.Pct), fmt.Sprintf("%s %.2f / %.2f", sp.Currency, *sp.Used, *sp.Limit), sp.Pct})
		}
		return out
	}
	if rl := a.RateLimits; rl != nil {
		if rl.FiveHourPct != nil {
			out = append(out, meterLine{"5-hour", pct(*rl.FiveHourPct), strings.TrimSpace(reset(rl.FiveHourReset) + " (headers)"), *rl.FiveHourPct})
		}
		if rl.SevenDayPct != nil {
			out = append(out, meterLine{"7-day", pct(*rl.SevenDayPct), strings.TrimSpace(reset(rl.SevenDayReset) + " (headers)"), *rl.SevenDayPct})
		}
	}
	return out
}

func pct(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) + "%" }

func countdown(t, now time.Time) string {
	if d := t.Sub(now); d > 0 {
		return humanDur(d)
	}
	return "now"
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// palette renders ANSI styles when on (a terminal without NO_COLOR).
type palette struct{ on bool }

func (p palette) wrap(code, s string) string {
	if !p.on {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}
func (p palette) bold(s string) string  { return p.wrap("1", s) }
func (p palette) dim(s string) string   { return p.wrap("2", s) }
func (p palette) green(s string) string { return p.wrap("32", s) }
func (p palette) amber(s string) string { return p.wrap("33", s) }
func (p palette) red(s string) string   { return p.wrap("31", s) }
func (p palette) blue(s string) string  { return p.wrap("34", s) }

// tone colors s by utilization: green below 70%, amber below 90%, red above.
func (p palette) tone(v float64, s string) string {
	switch {
	case v >= 90:
		return p.red(s)
	case v >= 70:
		return p.amber(s)
	default:
		return p.green(s)
	}
}

// bar draws a 20-cell meter.
func (p palette) bar(v float64) string {
	const width = 20
	n := int(math.Round(math.Max(0, math.Min(100, v)) / 100 * width))
	return p.tone(v, strings.Repeat("█", n)) + p.dim(strings.Repeat("░", width-n))
}

func (p palette) state(a proxy.UsageAccount, now time.Time) string {
	st := baseState(a, now)
	switch {
	case a.Disabled:
		st = p.dim("○ " + st)
	case a.Invalid:
		st = p.red("● " + st)
	case strings.HasPrefix(st, "cooling"):
		st = p.amber("● " + st)
	default:
		st = p.green("● available")
	}
	if a.InFlight > 0 && !a.Disabled && !a.Invalid {
		st += p.green(fmt.Sprintf(" · %d streaming", a.InFlight))
	}
	return st
}

// accountState labels an account, with its cooldown countdown when cooling.
func accountState(a proxy.UsageAccount, now time.Time) string {
	if a.InFlight > 0 && !a.Disabled && !a.Invalid {
		return fmt.Sprintf("%s · %d streaming", baseState(a, now), a.InFlight)
	}
	return baseState(a, now)
}

func baseState(a proxy.UsageAccount, now time.Time) string {
	switch {
	case a.Disabled:
		return "disabled"
	case a.Invalid:
		return "invalid"
	case a.CoolingUntil != nil && a.CoolingUntil.After(now):
		return "cooling " + humanDur(a.CoolingUntil.Sub(now))
	default:
		return "ok"
	}
}

func agoString(t *time.Time, now time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	return humanDur(now.Sub(*t)) + " ago"
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// humanDur renders a duration like the dashboard does (47m, 1h 32m).
func humanDur(d time.Duration) string { return proxy.HumanDuration(d) }

// envValue reads one variable from an environ-style slice.
func envValue(environ []string, name string) string {
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v
		}
	}
	return ""
}

// listening reports whether anything accepts connections on addr.
func listening(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}
