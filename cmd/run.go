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
	"time"

	"github.com/spf13/cobra"

	"github.com/dehydr8/cproxy/internal/config"
	"github.com/dehydr8/cproxy/internal/launch"
	"github.com/dehydr8/cproxy/internal/pool"
	"github.com/dehydr8/cproxy/internal/proxy"
	"github.com/dehydr8/cproxy/internal/version"
)

var (
	runPool     string
	runStrategy string
	runUpstream string
	runPort     int
)

var runCmd = &cobra.Command{
	Use:   "run [flags] [--] [command [args...]]",
	Short: "Start a local proxy and launch a command (default: claude) through it",
	Long: `Start a local proxy that fronts the Anthropic API, then launch COMMAND
(default: claude) with ANTHROPIC_BASE_URL and ANTHROPIC_AUTH_TOKEN pointed
at it. Ambient Anthropic credentials and CPROXY_* secrets are stripped from
the child's environment, so the selected pool is the only thing it can use.

The child shares cproxy's process group: Ctrl+C reaches it directly and
cproxy keeps serving until it exits, then exits with its code.

Examples:

	cproxy run -- claude                    # default pool, default command
	cproxy run --pool cvp -- claude --resume x
	CPROXY_TOKENS=a:sk-…,b:sk-… cproxy run -- claude
	cproxy run --pool cvp -- claude -p "hello"`,
	Args: cobra.ArbitraryArgs,
	RunE: runRun,
}

func init() {
	runCmd.Flags().StringVarP(&runPool, "pool", "p", "",
		"pool to draw accounts from (default: the \"default\" pool)")
	runCmd.Flags().StringVar(&runStrategy, "strategy", "",
		"override the pool's strategy ("+pool.Strategies()+")")
	runCmd.Flags().StringVar(&runUpstream, "upstream", "",
		"upstream base URL (default $CPROXY_UPSTREAM or "+config.DefaultUpstream+")")
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
	file, err := config.Load()
	if err != nil {
		return err
	}
	rt, err := config.Build(file, os.Environ())
	if err != nil {
		return err
	}
	members, strategy, err := rt.ResolvePool(runPool)
	if err != nil {
		return fmt.Errorf("%w\nadd accounts with: cproxy accounts add NAME --pool %s",
			err, poolFlagOrDefault())
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

	srv := proxy.New(proxy.Options{Upstream: upstream, State: st, Log: newLogger()})
	httpSrv := &http.Server{Handler: srv}
	go httpSrv.Serve(ln)

	log := newLogger()
	log.Info("cproxy started",
		"version", version.String(),
		"listen", baseURL,
		"pool", poolFlagOrDefault(),
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

// poolFlagOrDefault returns the effective pool name for messages.
func poolFlagOrDefault() string {
	if runPool != "" {
		return runPool
	}
	return config.DefaultPoolName
}

// childCommand resolves the wrapped command: everything after "--" (or all
// positional args), defaulting to claude on PATH.
func childCommand(cmd *cobra.Command, args []string) ([]string, error) {
	child := args
	if d := cmd.ArgsLenAtDash(); d >= 0 {
		child = args[d:]
	}
	if len(child) == 0 {
		child = []string{"claude"}
	}
	path, err := exec.LookPath(child[0])
	if err != nil {
		return nil, fmt.Errorf("%q not found on PATH; pass an explicit command after \"--\" (e.g. cproxy run -- claude)", child[0])
	}
	child[0] = path
	return child, nil
}

// randomToken returns a fresh "cp_…" client token.
func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return "cp_" + hex.EncodeToString(b), nil
}
