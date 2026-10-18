package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dehydr8/cproxy/internal/config"
	"github.com/dehydr8/cproxy/internal/launch"
	"github.com/dehydr8/cproxy/internal/pool"
	"github.com/dehydr8/cproxy/internal/proxy"
	"github.com/dehydr8/cproxy/internal/version"
	"github.com/dehydr8/cproxy/internal/wrap"
)

var (
	runPool     string
	runStrategy string
	runUpstream string
	runPort     int
	runIsolated bool
)

var runCmd = &cobra.Command{
	Use:   "run [flags] [--] [command [args...]]",
	Short: "Launch a command (default: claude) through the local cproxy daemon",
	Long: `Launch COMMAND (default: claude) with ANTHROPIC_BASE_URL and
ANTHROPIC_AUTH_TOKEN pointed at the per-user cproxy daemon, starting it on
first use. Every ` + "`cproxy run`" + ` and every wrapped claude on the machine share that
one daemon: one set of sticky pins, cooldowns and usage, one dashboard.
Launches drawing from the same pool share one identity (and so one sticky
account). Ambient Anthropic credentials and CPROXY_* secrets are stripped
from the child's environment.

Arguments after "--" that start with a flag (or are not a command on PATH)
are passed to claude, so ` + "`cproxy run --pool cvp -- --resume x`" + ` works like
cswap's run.

--isolated (implied by --strategy, --upstream or --port) runs a private
proxy for this one launch instead.

The child shares cproxy's process group: Ctrl+C reaches it directly, and
cproxy exits with its code.

Examples:

	cproxy run                               # claude, default pool
	cproxy run --pool cvp -- --resume x      # claude --resume x on pool cvp
	cproxy run --pool cvp -- claude -p "hello"
	cproxy run --isolated -- claude          # a private one-off proxy`,
	Args: cobra.ArbitraryArgs,
	RunE: runRun,
}

func init() {
	runCmd.Flags().StringVarP(&runPool, "pool", "p", "",
		"pool to draw accounts from (default $CPROXY_POOL, else \"default\")")
	runCmd.Flags().StringVar(&runStrategy, "strategy", "",
		"override the pool's strategy ("+pool.Strategies()+")")
	runCmd.Flags().StringVar(&runUpstream, "upstream", "",
		"upstream base URL (default $CPROXY_UPSTREAM or "+config.DefaultUpstream+")")
	runCmd.Flags().BoolVar(&runIsolated, "isolated", false,
		"run a private proxy for this launch instead of using the shared daemon")
	runCmd.Flags().IntVar(&runPort, "port", 0,
		"local port to listen on (default: pick a free one)")
	// Flags after the first positional argument belong to the wrapped
	// command, but an explicit "--" is always honored.
	runCmd.Flags().SetInterspersed(false)
}

func runRun(cmd *cobra.Command, args []string) error {
	if _, err := pool.ParseStrategy(runStrategy); err != nil {
		return err
	}
	if !runIsolated && runStrategy == "" && runUpstream == "" && !cmd.Flags().Changed("port") {
		return runViaDaemon(cmd, args)
	}
	file, err := config.Load()
	if err != nil {
		return err
	}
	rt, err := config.Build(file, os.Environ())
	if err != nil {
		return err
	}
	if runPool != "" {
		rt.Pool = runPool // --pool beats CPROXY_POOL
	}
	members, strategy, err := rt.ResolvePool("")
	if err != nil {
		return fmt.Errorf("%w\nadd accounts with: cproxy accounts add NAME --pool %s",
			err, rt.SelectedPool())
	}
	if runStrategy != "" {
		strategy, _ = pool.ParseStrategy(runStrategy)
	}

	child, err := childCommand(cmd, args)
	if err != nil {
		return err
	}

	upstream, err := resolveUpstream(runUpstream)
	if err != nil {
		return err
	}
	policy, err := config.SwitchPolicy(os.Environ())
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", runPort))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	sessionToken, err := randomToken()
	if err != nil {
		return err
	}

	// This session exposes only the resolved pool members; membership is
	// flattened to the default pool inside the proxy state.
	local := make([]pool.Account, len(members))
	for i, m := range members {
		m.Pools = nil
		local[i] = m
	}
	st := &proxy.State{
		Runtime: &config.Runtime{
			Accounts:    local,
			Pools:       map[string]config.PoolConfig{config.DefaultPoolName: {Strategy: string(strategy)}},
			ClientAuth:  map[string]config.ClientAuth{},
			DefaultPool: config.DefaultPoolName,
		},
		SessionToken: sessionToken,
	}

	// The private proxy shares this process (and so the terminal) with the
	// child: it logs to ~/.cproxy/run.log, never to the screen claude draws on.
	log := fileLogger("run.log")
	srv := proxy.New(proxy.Options{Upstream: upstream, State: st, Log: log})
	srv.Selector().SetSwitchPolicy(policy)
	httpSrv := &http.Server{Handler: srv}
	go httpSrv.Serve(ln)

	log.Info("cproxy started",
		"version", version.String(),
		"listen", baseURL,
		"pool", rt.SelectedPool(),
		"accounts", len(members),
		"strategy", string(strategy),
		"upstream", upstream.String(),
		"command", fmt.Sprintf("%q", child))

	childEnv := launch.BuildChildEnv(os.Environ(), baseURL, sessionToken)
	code, err := launch.Run(child, childEnv)

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpSrv.Shutdown(shutCtx)
	srv.Close()

	if err != nil {
		return err
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}

// childCommand resolves the wrapped command: everything after "--" (or all
// positional args), defaulting to claude on PATH.
//
// Arguments that start with a flag, or whose first word is not a command
// on PATH, are claude's: `cproxy run -- --resume x` runs `claude --resume x`,
// like cswap's run. claude resolves to the real binary even when the cproxy
// shim is installed, skipping a pointless second hop.
func childCommand(cmd *cobra.Command, args []string) ([]string, error) {
	child := args
	if d := cmd.ArgsLenAtDash(); d >= 0 {
		child = args[d:]
	}
	if len(child) > 0 && child[0] != "claude" && !strings.HasPrefix(child[0], "-") {
		if path, err := exec.LookPath(child[0]); err == nil {
			return append([]string{path}, child[1:]...), nil
		}
	}
	if len(child) > 0 && child[0] == "claude" {
		child = child[1:]
	}
	real, err := resolveRealClaude("")
	if err != nil {
		return nil, err
	}
	return append([]string{real}, child...), nil
}

// runViaDaemon launches the child through the shared per-user daemon. All
// launches drawing from the same pool share one identity, named
// claude-local (or claude-local@<pool> for an explicit --pool).
func runViaDaemon(cmd *cobra.Command, args []string) error {
	child, err := childCommand(cmd, args)
	if err != nil {
		return err
	}
	c, st, err := wrap.EnsureDaemon(os.Environ())
	if err != nil {
		return err
	}
	name := config.LocalClientName
	if runPool != "" {
		name += "@" + runPool
	}
	token, err := c.Register(name, runPool)
	if err != nil {
		return fmt.Errorf("%w\nadd accounts with: cproxy accounts add NAME --pool %s", err, runPool)
	}
	baseURL := "http://" + st.Addr
	fileLogger("run.log").Info("cproxy run", "version", version.String(), "proxy", baseURL,
		"daemon", st.Version, "client", name, "command", fmt.Sprintf("%q", child))
	code, err := launch.Run(child, launch.BuildChildEnv(os.Environ(), baseURL, token))
	if err != nil {
		return err
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}

// randomToken returns a fresh "cp_…" client token.
func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return "cp_" + hex.EncodeToString(b), nil
}
