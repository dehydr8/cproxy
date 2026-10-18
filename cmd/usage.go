package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dehydr8/cproxy/internal/config"
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
	Long: `Print the live usage of a running cproxy serve instance
(127.0.0.1:7788 by default; $CPROXY_PORT or --port) in the terminal: pool
accounts with request counts, rate-limit state, real usage numbers from
Anthropic's usage API (tokens with the user:profile scope; preferred) and
the last anthropic-ratelimit-* headers cproxy observed upstream (the
labeled fallback), and clients with their sticky pins. --json prints the
raw dashboard JSON instead.

The instance is whatever ` + "`cproxy serve`" + ` or the claude wrapper started. A
one-shot ` + "`cproxy run`" + ` session keeps its port and its session token to
itself and cannot be queried.

The client token is picked from the usual places and must carry the usage
scope (tokens with no scopes may view it too): $CPROXY_AUTH_TOKEN, a
matching $ANTHROPIC_AUTH_TOKEN, the first usage-capable token in
$CPROXY_CLIENT_TOKENS, then credentials.json by client name. Generate a
dashboard-only one with ` + "`cproxy token --scopes usage`" + `.`,
	RunE: runUsageCmd,
}

func init() {
	usageCmd.Flags().IntVar(&usagePortFlag, "port", defaultPort(),
		"port of the running cproxy (default $CPROXY_PORT or 7788)")
	usageCmd.Flags().BoolVar(&usageRawJSON, "json", false,
		"print the raw /usage JSON instead of a table")
}

func runUsageCmd(cmd *cobra.Command, args []string) error {
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
			return fmt.Errorf("no cproxy is answering on %s (is `cproxy serve` or the claude wrapper running? start one with `cproxy serve` or launch claude through the wrapper): %w", addr, err)
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
	if usageRawJSON {
		os.Stdout.Write(body)
		return nil
	}

	var snap proxy.UsageSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return err
	}
	return renderUsage(os.Stdout, snap, addr, time.Now())
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

// renderUsage prints a snapshot as aligned tables.
func renderUsage(out io.Writer, snap proxy.UsageSnapshot, addr string, now time.Time) error {
	w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	fmt.Fprintf(w, "cproxy %s on %s\tupstream %s\tup %s\n",
		snap.Version, addr, snap.Upstream, humanDur(time.Duration(snap.UptimeSeconds*float64(time.Second))))

	fmt.Fprintf(w, "\naccounts\t\n")
	fmt.Fprintf(w, "  name\tstate\treq\ttokens\t429\terr\tlast used\tusage\trate limits\n")
	for _, a := range snap.Accounts {
		name := a.Name
		if a.Email != "" {
			name += " (" + a.Email + ")"
		}
		tokens, _ := proxy.TokensLabel(a.Tokens)
		fmt.Fprintf(w, "  %s\t%s\t%d\t%s\t%d\t%d\t%s\t%s\t%s\n",
			name, accountState(a, now), a.Requests, orDash(tokens), a.RateLimited, a.Errors,
			agoString(a.LastUsed, now),
			proxy.UsageAPISummary(a.UsageAPI, a.UsageAPIErr, now),
			proxy.RateLimitsSummary(a.RateLimits, now))
	}

	fmt.Fprintf(w, "\nclients\t\n")
	fmt.Fprintf(w, "  name\tpool\tpinned\treq\ttokens\tlast seen\n")
	for _, c := range snap.Clients {
		tokens, _ := proxy.TokensLabel(c.Tokens)
		seen := agoString(c.LastSeen, now)
		if c.Live > 0 {
			seen = "streaming"
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%d\t%s\t%s\n",
			c.Name, c.Pool, orDash(c.Pinned), c.Requests, orDash(tokens), seen)
	}
	return w.Flush()
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
