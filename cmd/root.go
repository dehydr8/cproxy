// Package cmd defines the cproxy CLI.
package cmd

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"

	"github.com/spf13/cobra"

	"github.com/dehydr8/cproxy/internal/config"
)

var quiet bool

var rootCmd = &cobra.Command{
	Use:   "cproxy",
	Short: "Proxy for the Anthropic API that pools multiple Claude credentials",
	Long: `cproxy runs a proxy in front of the Anthropic API and controls the
credential sent upstream, so several Claude subscriptions can back a single
Claude CLI (a "pool"). It never touches Claude's own files (~/.claude,
.credentials.json); the only files it owns live under ~/.cproxy/.

Credentials live in ~/.cproxy/credentials.json ("accounts", optionally
grouped into "pools") or in the environment:

	CPROXY_TOKEN_MAIN=sk-ant-oat01-…           # one account per variable
	CPROXY_POOL_TOKEN_CVP_A=sk-ant-oat01-…     # account "cvp-a" in pool cvp
	CPROXY_POOL=cvp                            # pool used without --pool
	CPROXY_TOKENS=main:sk-ant-oat01-…,alt:…    # or several in one list

Quick start:

	cproxy accounts add work --pool cvp
	cproxy run --pool cvp -- claude       # local: Claude talks to cproxy
	cproxy serve --addr :7788            # shared: others point at you
	cproxy install                       # or: make the claude command itself go through it

By default each client sticks to one pool account and only switches when
that account hits its limit, so a conversation pays for its full context on
exactly one subscription instead of on every member of the pool.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rootCmd.PersistentFlags().BoolVarP(&quiet, "quiet", "q", false,
		"suppress per-request logs (errors still print)")
	rootCmd.AddCommand(runCmd, serveCmd, claudeCmd, installCmd, uninstallCmd,
		usageCmd, accountsCmd, poolsCmd, tokenCmd, setupTokenCmd, versionCmd)
}

// Execute runs the CLI; errors are printed and exit non-zero.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "cproxy:", err)
		os.Exit(1)
	}
}

// newLogger builds the process logger. cproxy logs to stderr so stdout stays
// clean for the wrapped command in run mode.
func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if quiet {
		level = slog.LevelWarn
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// resolveUpstream picks the upstream base URL from a flag, else
// CPROXY_UPSTREAM, else the Anthropic default.
func resolveUpstream(flagVal string) (*url.URL, error) {
	raw := flagVal
	if raw == "" {
		raw = config.EnvUpstream(os.Environ())
	}
	if raw == "" {
		raw = config.DefaultUpstream
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream URL %q: %w", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid upstream URL %q (want scheme://host)", raw)
	}
	return u, nil
}
