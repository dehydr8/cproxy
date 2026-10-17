package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dehydr8/cproxy/internal/config"
	"github.com/dehydr8/cproxy/internal/launch"
	"github.com/dehydr8/cproxy/internal/pool"
	"github.com/dehydr8/cproxy/internal/proxy"
	"github.com/dehydr8/cproxy/internal/version"
)

var (
	serveAddr         string
	servePool         string
	serveStrategy     string
	serveUpstream     string
	serveClientTokens string
	serveTLSCert      string
	serveTLSKey       string
	serveNoAuth       bool
)

var serveCmd = &cobra.Command{
	Use:   "serve [flags]",
	Short: "Run cproxy as a shared server with client-token auth",
	Long: `Run cproxy as a long-lived server that multiple people can point their
Claude CLIs at. Clients set:

	ANTHROPIC_BASE_URL=http://your-host:7788
	ANTHROPIC_AUTH_TOKEN=<their cproxy client token>

and are authenticated against client tokens from --client-tokens,
$CPROXY_CLIENT_TOKENS or the client_tokens map in credentials.json.
Each client token may optionally route to its own pool:

	"client_tokens": {"alice": {"token": "cp_…", "pool": "cvp"}}

Client tokens never travel upstream: cproxy replaces the Authorization
header with a pool account's credential. Send SIGHUP to reload
credentials.json without dropping connections. Run behind TLS (--tls-cert /
--tls-key) or a terminating proxy when exposing this beyond localhost.`,
	RunE: runServe,
}

func init() {
	serveCmd.Flags().StringVar(&serveAddr, "addr", ":7788", "address to listen on")
	serveCmd.Flags().StringVarP(&servePool, "pool", "p", "",
		"pool to draw accounts from (default: the \"default\" pool)")
	serveCmd.Flags().StringVar(&serveStrategy, "strategy", "",
		"override the pool's strategy ("+pool.Strategies()+")")
	serveCmd.Flags().StringVar(&serveUpstream, "upstream", "",
		"upstream base URL (default $CPROXY_UPSTREAM or "+config.DefaultUpstream+")")
	serveCmd.Flags().StringVar(&serveClientTokens, "client-tokens", "",
		"serve-mode client tokens as name:token,name:token (overrides same-named file/env entries)")
	serveCmd.Flags().StringVar(&serveTLSCert, "tls-cert", "", "TLS certificate file (optional)")
	serveCmd.Flags().StringVar(&serveTLSKey, "tls-key", "", "TLS private key file (optional)")
	serveCmd.Flags().BoolVar(&serveNoAuth, "no-auth", false,
		"accept unauthenticated clients (dangerous; localhost only)")
}

func runServe(cmd *cobra.Command, args []string) error {
	if _, err := pool.ParseStrategy(serveStrategy); err != nil {
		return err
	}
	st, rt, err := buildServeState()
	if err != nil {
		return err
	}

	upstream, err := resolveUpstream(serveUpstream)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", serveAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	srv := proxy.New(proxy.Options{Upstream: upstream, State: st, Log: newLogger()})
	httpSrv := &http.Server{Handler: srv}
	log := newLogger()

	scheme := "http"
	if serveTLSCert != "" || serveTLSKey != "" {
		if serveTLSCert == "" || serveTLSKey == "" {
			return errors.New("--tls-cert and --tls-key must be used together")
		}
		scheme = "https"
	}

	poolLabel := servePool
	if poolLabel == "" {
		poolLabel = config.DefaultPoolName
	}
	log.Info("cproxy serving",
		"version", version.String(),
		"listen", scheme+"://"+ln.Addr().String(),
		"pool", poolLabel,
		"accounts", len(rt.Accounts),
		"clients", len(rt.ClientAuth),
		"no_auth", serveNoAuth,
		"upstream", upstream.String())

	// SIGHUP reloads credentials.json (and the environment) without
	// dropping connections; health state (cooldowns) survives.
	hup := make(chan os.Signal, 1)
	launch.NotifyHUP(hup)
	go func() {
		for range hup {
			reloadServe(srv, rt)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		log.Info("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpSrv.Shutdown(ctx)
	}()

	if scheme == "https" {
		err = httpSrv.ServeTLS(ln, serveTLSCert, serveTLSKey)
	} else {
		err = httpSrv.Serve(ln)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// buildServeState resolves the file + env + flag configuration into the
// proxy's request-time state.
func buildServeState() (*proxy.State, *config.Runtime, error) {
	file, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	rt, err := config.Build(file, os.Environ())
	if err != nil {
		return nil, nil, err
	}

	if serveClientTokens != "" {
		for name, spec := range config.ClientTokensFromList(serveClientTokens) {
			for tok, ca := range rt.ClientAuth {
				if ca.Name == name {
					delete(rt.ClientAuth, tok)
				}
			}
			rt.ClientAuth[spec.Token] = config.ClientAuth{Name: name, Pool: spec.Pool}
		}
	}
	if len(rt.ClientAuth) == 0 && !serveNoAuth {
		return nil, nil, fmt.Errorf(
			"serve mode requires client tokens; configure them via --client-tokens 'name:cp_…', "+
				"$%s, or client_tokens in %s (generate one: cproxy token). "+
				"Pass --no-auth only for localhost testing",
			config.EnvClientTokens, config.Path())
	}

	if serveStrategy != "" {
		pn := servePool
		if pn == "" {
			pn = config.DefaultPoolName
		}
		if rt.Pools == nil {
			rt.Pools = map[string]config.PoolConfig{}
		}
		pc := rt.Pools[pn]
		pc.Strategy = serveStrategy
		rt.Pools[pn] = pc
	}

	if _, _, err := rt.ResolvePool(servePool); err != nil {
		return nil, nil, err
	}
	return &proxy.State{Runtime: rt, NoAuth: serveNoAuth}, rt, nil
}

// reloadServe rebuilds the state on SIGHUP and swaps it in, keeping the old
// one on any error. Health state for accounts whose token changed is reset.
func reloadServe(srv *proxy.Server, old *config.Runtime) {
	log := newLogger()
	st, rt, err := buildServeState()
	if err != nil {
		log.Error("reload failed; keeping previous config", "err", err.Error())
		return
	}
	oldByName := map[string]pool.Account{}
	if old != nil {
		for _, a := range old.Accounts {
			oldByName[a.Name] = a
		}
	}
	for _, a := range rt.Accounts {
		if prev, ok := oldByName[a.Name]; ok && prev.Token != a.Token {
			srv.Selector().ClearAccount(a.Name)
		}
	}
	srv.Update(st)
	log.Info("reloaded", "accounts", len(rt.Accounts), "clients", len(rt.ClientAuth))
}
