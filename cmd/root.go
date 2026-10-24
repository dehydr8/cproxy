// Package cmd defines the cproxy CLI.
package cmd

import (
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"

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
	cproxy run --pool cvp                 # local: claude through the per-user daemon
	cproxy install                        # or: make the claude command itself go through it
	cproxy usage                          # how the pool is doing (or: cproxy dashboard)
	cproxy serve --addr :7788             # shared: others point at you

By default each client sticks to one pool account and moves on just before
that account runs out (CPROXY_SWITCH_AT, default 90%), so a conversation
pays for its full context on exactly one subscription at a time.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rootCmd.PersistentFlags().BoolVarP(&quiet, "quiet", "q", false,
		"suppress per-request logs (errors still print)")
	rootCmd.AddCommand(runCmd, serveCmd, daemonCmd, dashboardCmd, claudeCmd, installCmd, uninstallCmd,
		usageCmd, accountsCmd, poolsCmd, tokenCmd, setupTokenCmd, updateCmd, versionCmd)
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

// fileLogger logs to a file under ~/.cproxy. Commands that share the
// terminal with claude (run, the claude wrapper) must never write to it once
// claude is drawing its interface there.
func fileLogger(name string) *slog.Logger {
	level := slog.LevelInfo
	if quiet {
		level = slog.LevelWarn
	}
	if err := os.MkdirAll(config.Home(), 0o700); err == nil {
		if f, err := os.OpenFile(filepath.Join(config.Home(), name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			return slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: level}))
		}
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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
