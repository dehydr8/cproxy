package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
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
	serveUsageAddr    string
)

var serveCmd = &cobra.Command{
	Use:   "serve [flags]",
	Short: "Run cproxy as a shared server with client-token auth",
	Long: `Run cproxy as a long-lived server that multiple people can point their
Claude CLIs at. Clients set:

	ANTHROPIC_BASE_URL=http://your-host:7788
	ANTHROPIC_AUTH_TOKEN=<their cproxy client token>

and are authenticated against client tokens from --client-tokens,
$CPROXY_CLIENT_TOKEN_<NAME>, $CPROXY_CLIENT_TOKENS or the client_tokens map
in credentials.json.
Each client token may optionally route to its own pool:

	"client_tokens": {"alice": {"token": "cp_…", "pool": "cvp"}}

Client tokens never travel upstream: cproxy replaces the Authorization
header with a pool account's credential. Changes to credentials.json are
picked up automatically (or send SIGHUP) without dropping connections;
environment variables are read once at startup. Run behind TLS (--tls-cert /
--tls-key) or a terminating proxy when exposing this beyond localhost.`,
	RunE: runServe,
}

func init() {
	serveCmd.Flags().StringVar(&serveAddr, "addr", ":7788", "address to listen on")
	serveCmd.Flags().StringVarP(&servePool, "pool", "p", "",
		"pool for clients that do not name their own (default $CPROXY_POOL, else \"default\")")
	serveCmd.Flags().StringVar(&serveStrategy, "strategy", "",
		"override the pool's strategy ("+pool.Strategies()+")")
	serveCmd.Flags().StringVar(&serveUpstream, "upstream", "",
		"upstream base URL (default $CPROXY_UPSTREAM or "+config.DefaultUpstream+")")
	serveCmd.Flags().StringVar(&serveClientTokens, "client-tokens", "",
		"serve-mode client tokens as name:token,name:token (overrides same-named file/env entries)")
	serveCmd.Flags().StringVar(&serveTLSCert, "tls-cert", "", "TLS certificate file (optional)")
	serveCmd.Flags().StringVar(&serveTLSKey, "tls-key", "", "TLS private key file (optional)")
	serveCmd.Flags().StringVar(&serveUsageAddr, "usage-addr", "",
		"serve the /usage dashboard on this separate address only (e.g. a private network), keeping it off --addr")
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
	policy, err := config.SwitchPolicy(os.Environ())
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", serveAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	srv := proxy.New(proxy.Options{Upstream: upstream, State: st, Log: newLogger(), HideUsage: serveUsageAddr != ""})
	srv.Selector().SetSwitchPolicy(policy)
	httpSrv := &http.Server{Handler: srv, ReadHeaderTimeout: 30 * time.Second}
	log := newLogger()

	// --usage-addr: the dashboard gets its own (private) listener and the
	// public one serves only the API.
	var usageSrv *http.Server
	if serveUsageAddr != "" {
		uln, err := net.Listen("tcp", serveUsageAddr)
		if err != nil {
			return fmt.Errorf("listen on --usage-addr: %w", err)
		}
		usageSrv = &http.Server{Handler: srv.UsageHandler(), ReadHeaderTimeout: 30 * time.Second}
		go usageSrv.Serve(uln)
		log.Info("usage dashboard on its own listener", "listen", "http://"+uln.Addr().String()+"/usage")
	}

	scheme := "http"
	if serveTLSCert != "" || serveTLSKey != "" {
		if serveTLSCert == "" || serveTLSKey == "" {
			return errors.New("--tls-cert and --tls-key must be used together")
		}
		scheme = "https"
	}

	poolLabel := rt.SelectedPool()
	log.Info("cproxy serving",
		"version", version.String(),
		"listen", scheme+"://"+ln.Addr().String(),
		"pool", poolLabel,
		"accounts", len(rt.Accounts),
		"clients", len(rt.ClientAuth),
		"no_auth", serveNoAuth,
		"upstream", upstream.String())

	// Editing credentials.json (e.g. `cproxy accounts add`) or SIGHUP
	// reloads without dropping connections; health state (cooldowns)
	// survives. The environment is fixed for the life of the process.
	hup := make(chan os.Signal, 1)
	launch.NotifyHUP(hup)
	go func() {
		cur := rt
		watchReloads(hup, []string{config.Path(), config.LocalTokenPath(), config.DashboardTokenPath()}, credentialsPoll, func() {
			if next := reloadServe(srv, cur); next != nil {
				cur = next
			}
		})
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		log.Info("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpSrv.Shutdown(ctx)
		if usageSrv != nil {
			usageSrv.Shutdown(ctx)
		}
		srv.Close()
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

// credentialsPoll is how often serve checks credentials.json for changes.
const credentialsPoll = 2 * time.Second

// fileStamp identifies one version of a file; the zero value means missing.
type fileStamp struct {
	mod  time.Time
	size int64
}

func stampOf(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{fi.ModTime(), fi.Size()}
}

// watchReloads calls reload on every signal from hup and whenever one of
// paths changes (checked every interval). It returns when hup is closed.
func watchReloads(hup <-chan os.Signal, paths []string, every time.Duration, reload func()) {
	stamps := func() []fileStamp {
		out := make([]fileStamp, len(paths))
		for i, p := range paths {
			out[i] = stampOf(p)
		}
		return out
	}
	last := stamps()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case _, ok := <-hup:
			if !ok {
				return
			}
		case <-t.C:
			if slices.Equal(stamps(), last) {
				continue
			}
			newLogger().Info("credential files changed; reloading")
		}
		last = stamps()
		reload()
	}
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
			scopes, err := config.ParseScopes(spec.Scopes)
			if err != nil {
				return nil, nil, fmt.Errorf("--client-tokens %q: %w", name, err)
			}
			for tok, ca := range rt.ClientAuth {
				if ca.Name == name {
					delete(rt.ClientAuth, tok)
				}
			}
			rt.ClientAuth[spec.Token] = config.ClientAuth{Name: name, Pool: spec.Pool, Scopes: scopes}
		}
	}
	if len(rt.ClientAuth) == 0 && !serveNoAuth {
		return nil, nil, fmt.Errorf(
			"serve mode requires client tokens; configure them via --client-tokens 'name:cp_…', "+
				"$%s_<NAME>, or client_tokens in %s (generate one: cproxy token). "+
				"Pass --no-auth only for localhost testing",
			strings.TrimSuffix(config.EnvClientTokenPrefix, "_"), config.Path())
	}

	// --pool beats CPROXY_POOL; either one routes every client that does
	// not name its own pool.
	if servePool != "" {
		rt.Pool = servePool
	}
	if serveStrategy != "" {
		pn := rt.SelectedPool()
		if rt.Pools == nil {
			rt.Pools = map[string]config.PoolConfig{}
		}
		pc := rt.Pools[pn]
		pc.Strategy = serveStrategy
		rt.Pools[pn] = pc
	}

	if _, _, err := rt.ResolvePool(""); err != nil {
		return nil, nil, err
	}
	return &proxy.State{Runtime: rt, NoAuth: serveNoAuth}, rt, nil
}

// reloadServe rebuilds the state on SIGHUP and swaps it in, keeping the old
// one on any error. Health state for accounts whose token changed is reset.
// It returns the new runtime (nil on error) so the next reload diffs
// against what is actually being served.
func reloadServe(srv *proxy.Server, old *config.Runtime) *config.Runtime {
	log := newLogger()
	st, rt, err := buildServeState()
	if err != nil {
		log.Error("reload failed; keeping previous config", "err", err.Error())
		return nil
	}
	oldByName := map[string]pool.Account{}
	if old != nil {
		for _, a := range old.Accounts {
			oldByName[a.Name] = a
		}
	}
	for _, a := range rt.Accounts {
		if prev, ok := oldByName[a.Name]; ok && prev.Token != a.Token {
			srv.ForgetAccount(a.Name)
		}
	}
	srv.Update(st)
	log.Info("reloaded", "accounts", len(rt.Accounts), "clients", len(rt.ClientAuth))
	return rt
}
