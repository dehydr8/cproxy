package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/dehydr8/cproxy/internal/config"
	"github.com/dehydr8/cproxy/internal/launch"
	"github.com/dehydr8/cproxy/internal/version"
	"github.com/dehydr8/cproxy/internal/wrap"
)

var (
	claudeExec string
	claudePort int
)

var claudeCmd = &cobra.Command{
	Use:   "claude [flags] [--] [claude args…]",
	Short: "Run the real claude through the shared local cproxy",
	Long: `Run the real claude binary with ANTHROPIC_BASE_URL and
ANTHROPIC_AUTH_TOKEN pointed at a shared local cproxy serve instance
(127.0.0.1:7788 by default; $CPROXY_PORT or --port), starting one when
nothing is listening. This is what the shim installed by ` + "`cproxy install`" + `
runs; it also works on its own.

Credentials come from the usual places (environment or
~/.cproxy/credentials.json). The client token claude presents is picked
from the configured client tokens: $CPROXY_AUTH_TOKEN, then a matching
$ANTHROPIC_AUTH_TOKEN, then the first proxy-scoped token in
$CPROXY_CLIENT_TOKENS, then credentials.json.

It backs off — running claude completely untouched — when nothing is
configured, or when the environment already carries its own Anthropic
routing (ANTHROPIC_BASE_URL, or an ANTHROPIC_AUTH_TOKEN that is not a
cproxy client token).

Examples:

  cproxy claude                       # claude, through cproxy
  cproxy claude -- -p "hello"         # same, with claude args
  cproxy install                      # make ` + "`claude`" + ` itself do this`,
	Args: cobra.ArbitraryArgs,
	RunE: runClaude,
}

func init() {
	claudeCmd.Flags().StringVar(&claudeExec, "exec", "",
		"path to the real claude binary (default: resolve from PATH, unwrapping the shim)")
	claudeCmd.Flags().IntVar(&claudePort, "port", defaultPort(),
		"port of the shared serve instance (default $CPROXY_PORT or 7788)")
	// Flags after the first positional argument belong to claude, but an
	// explicit "--" is always honored.
	claudeCmd.Flags().SetInterspersed(false)
}

// defaultPort resolves the shared serve port from $CPROXY_PORT.
func defaultPort() int {
	if p := config.EnvPort(os.Environ()); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return wrap.DefaultPort
}

func runClaude(cmd *cobra.Command, args []string) error {
	claudeArgs := args
	if d := cmd.ArgsLenAtDash(); d >= 0 {
		claudeArgs = args[d:]
	}
	real, err := resolveRealClaude(claudeExec)
	if err != nil {
		return err
	}
	argv := append([]string{real}, claudeArgs...)

	file, err := config.Load()
	if err != nil {
		return err
	}
	rt, err := config.Build(file, os.Environ())
	if err != nil {
		return err
	}

	plan, reason, token, _, err := wrap.Decide(rt, os.Environ())
	if err != nil {
		return err
	}
	if plan == wrap.PlanPassThrough {
		return runRealClaude(argv, "cproxy: "+reason+"; running claude directly")
	}

	if len(rt.Accounts) == 0 {
		return fmt.Errorf("a client token is configured but no pool accounts are; add accounts with `cproxy accounts add NAME` or set CPROXY_TOKENS (or run claude per-invocation with `cproxy run -- claude`)")
	}

	if _, err := wrap.EnsureServe(wrap.ServeOptions{Port: claudePort, Environ: os.Environ()}); err != nil {
		return err
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", claudePort)
	log := newLogger()
	log.Info("cproxy claude",
		"version", version.String(),
		"proxy", baseURL,
		"command", fmt.Sprintf("%q", argv[0]),
		"port", claudePort)

	env := launch.BuildChildEnv(os.Environ(), baseURL, token)
	code, err := launch.Run(argv, env)
	if err != nil {
		return err
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}

// runRealClaude launches claude with the environment exactly as cproxy
// found it.
func runRealClaude(argv []string, note string) error {
	if note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	code, err := launch.Run(argv, os.Environ())
	if err != nil {
		return err
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}

// resolveRealClaude finds the real claude binary: the --exec path, else
// `claude` on PATH, unwrapping a cproxy shim to its backed-up real binary.
func resolveRealClaude(flagVal string) (string, error) {
	p := flagVal
	if p == "" {
		found, err := exec.LookPath("claude")
		if err != nil {
			return "", errors.New("claude not found on PATH; install Claude Code or pass --exec / use cproxy install --path")
		}
		p = found
		if wrap.IsShim(p) {
			backup := wrap.BackupPath(p)
			if _, err := os.Stat(backup); err != nil {
				return "", fmt.Errorf("%s is the cproxy shim but its real binary %s is missing; restore with cproxy install", p, backup)
			}
			p = backup
		}
	}
	info, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("claude binary %s not found", p)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", p)
	}
	return p, nil
}

// discoverClaude finds the claude binary to wrap: --path, else PATH, else
// the locations Amp also checks (~/.local/bin, ~/bin).
func discoverClaude(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	for _, c := range []string{".local/bin/claude", "bin/claude"} {
		p := filepath.Join(home, filepath.FromSlash(c))
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("claude not found; pass --path /path/to/claude (cproxy needs to replace it with the shim)")
}
