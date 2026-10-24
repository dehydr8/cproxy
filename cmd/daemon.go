package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dehydr8/cproxy/internal/config"
	"github.com/dehydr8/cproxy/internal/daemon"
	"github.com/dehydr8/cproxy/internal/launch"
	"github.com/dehydr8/cproxy/internal/proxy"
	"github.com/dehydr8/cproxy/internal/version"
	"github.com/dehydr8/cproxy/internal/wrap"
)

// drainTimeout bounds how long a daemon handing over keeps finishing the
// requests (long Claude streams) it was serving.
const drainTimeout = 30 * time.Minute

var daemonCmd = &cobra.Command{
	Use:   "daemon [status|start|stop|restart|logs]",
	Short: "Run or control the per-user local daemon (started automatically)",
	Long: `Run the local cproxy daemon in the foreground. You normally never start it
yourself: ` + "`cproxy claude`" + ` (the installed shim) and ` + "`cproxy run`" + ` start it on
first use and every later launch shares it.

There is one daemon per user, guaranteed by a lock in ~/.cproxy. Claude talks
to it on 127.0.0.1 ($CPROXY_PORT, else 7788, else any free port); cproxy
commands talk to it over the private socket ~/.cproxy/cproxy.sock to read its
status and usage, hand it their CPROXY_* configuration (it reloads when that
changed), and hand over to a newer cproxy without dropping streams.

Control it with:

	cproxy daemon status     # running? version, pid, address, streams, log
	cproxy daemon start      # start it in the background
	cproxy daemon stop       # stop listening; open streams finish first
	cproxy daemon restart    # move to the installed cproxy, keeping its config
	cproxy daemon logs -f    # its log (~/.cproxy/daemon.log)`,
	Args: cobra.NoArgs,
	RunE: runDaemon,
}

func runDaemon(cmd *cobra.Command, args []string) error {
	log := newLogger()
	release, err := daemon.Lock()
	if errors.Is(err, daemon.ErrRunning) {
		log.Info("another cproxy daemon is running; exiting")
		return nil
	}
	if err != nil {
		return err
	}
	defer release()

	base := os.Environ()
	env := base
	st, rt, err := daemonState(env)
	if err != nil {
		return err
	}
	upstream, err := resolveUpstream("")
	if err != nil {
		return err
	}
	policy, err := config.SwitchPolicy(env)
	if err != nil {
		return err
	}

	// Claude's side: the preferred port, else any free one.
	port := defaultPort()
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		log.Warn("preferred port busy; using a free one", "port", port, "err", err.Error())
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return fmt.Errorf("listen: %w", err)
		}
	}
	addr := ln.Addr().String()

	// cproxy's side: the private socket. Holding the lock means any socket
	// file left behind is stale.
	sock := daemon.SocketPath()
	os.Remove(sock)
	ctl, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	os.Chmod(sock, 0o600)

	srv := proxy.New(proxy.Options{Upstream: upstream, State: st, Log: log, Probe: claudeUsageProbe()})
	srv.Selector().SetSwitchPolicy(policy)
	data := &http.Server{Handler: srv, ReadHeaderTimeout: 30 * time.Second}

	var mu sync.Mutex // guards env, rt, envHash
	envHash := daemon.EnvHash(env)
	reload := func(next []string) error {
		mu.Lock()
		defer mu.Unlock()
		nst, nrt, err := daemonState(next)
		if err != nil {
			return err
		}
		changedAccounts(srv, rt, nrt)
		srv.Update(nst)
		env, rt, envHash = next, nrt, daemon.EnvHash(next)
		return nil
	}

	started := time.Now()
	var shutdownOnce sync.Once
	done := make(chan struct{})
	// shutdown stops listening at once (a successor can take the port and
	// the lock), runs successor if set, then finishes the streams in flight.
	shutdown := func(successor func()) {
		shutdownOnce.Do(func() {
			log.Info("handing over; finishing in-flight requests")
			ctl.Close()
			os.Remove(sock)
			ln.Close() // free the port before the successor starts
			release()
			if successor != nil {
				successor()
			}
			ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
			defer cancel()
			data.Shutdown(ctx)
			srv.Close()
			close(done)
		})
	}

	registered := map[string]string{} // name -> token
	ctlSrv := &http.Server{Handler: daemon.Handler{
		Status: func() daemon.Status {
			mu.Lock()
			defer mu.Unlock()
			return daemon.Status{PID: os.Getpid(), Version: version.String(), Addr: addr, Started: started, EnvHash: envHash}
		},
		SyncEnv: func(cfg []string) (bool, error) {
			mu.Lock()
			same := daemon.EnvHash(cfg) == envHash
			changed := changedVars(daemon.ConfigEnv(env), cfg)
			mu.Unlock()
			if same {
				return false, nil
			}
			// Keep the daemon's own non-configuration environment (proxy
			// settings, HOME, …) and take the caller's CPROXY_* config.
			next := append(withoutConfigEnv(base), cfg...)
			if err := reload(next); err != nil {
				return false, err
			}
			// Names only: the values are secrets.
			log.Info("configuration updated by a client", "changed", strings.Join(changed, ","))
			return true, nil
		},
		Register: func(name, pool string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			if _, _, err := rt.ResolvePool(pool); err != nil {
				return "", err
			}
			key := name + "|" + pool
			if tok, ok := registered[key]; ok {
				return tok, nil
			}
			b := make([]byte, 24)
			rand.Read(b)
			tok := "cp_" + hex.EncodeToString(b)
			srv.AddClient(tok, config.ClientAuth{Name: name, Pool: pool})
			registered[key] = tok
			return tok, nil
		},
		Usage:    srv.UsageJSON(),
		Shutdown: func() { shutdown(nil) },
		Restart: func() {
			mu.Lock()
			cur := env
			mu.Unlock()
			shutdown(func() {
				if err := wrap.SpawnDaemon(cur); err != nil {
					log.Error("restart: starting the successor failed", "err", err.Error())
				}
			})
		},
	}}

	log.Info("cproxy daemon running", "version", version.String(), "listen", "http://"+addr,
		"socket", sock, "accounts", len(rt.Accounts), "pool", rt.SelectedPool(), "upstream", upstream.String())

	go ctlSrv.Serve(ctl)
	go data.Serve(ln)

	hup := make(chan os.Signal, 1)
	launch.NotifyHUP(hup)
	go watchReloads(hup, []string{config.Path(), config.LocalTokenPath(), config.DashboardTokenPath()}, credentialsPoll, func() {
		mu.Lock()
		cur := env
		mu.Unlock()
		if err := reload(cur); err != nil {
			log.Error("reload failed; keeping previous config", "err", err.Error())
		}
	})
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() { <-stop; shutdown(nil) }()

	<-done
	return nil
}

// daemonState resolves file + environment into the daemon's state. Unlike
// serve it needs no configured client tokens: local commands authenticate
// with the local token or register clients over the socket.
func daemonState(environ []string) (*proxy.State, *config.Runtime, error) {
	file, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	rt, err := config.Build(file, environ)
	if err != nil {
		return nil, nil, err
	}
	return &proxy.State{Runtime: rt}, rt, nil
}

// changedAccounts resets health state for accounts whose token changed.
func changedAccounts(srv *proxy.Server, old, next *config.Runtime) {
	prev := map[string]string{}
	for _, a := range old.Accounts {
		prev[a.Name] = a.Token
	}
	for _, a := range next.Accounts {
		if tok, ok := prev[a.Name]; ok && tok != a.Token {
			srv.ForgetAccount(a.Name)
		}
	}
}

// changedVars lists the names of variables added, removed or changed
// between two environments.
func changedVars(old, next []string) []string {
	vals := func(env []string) map[string]string {
		m := map[string]string{}
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			m[k] = v
		}
		return m
	}
	o, n := vals(old), vals(next)
	var out []string
	for k, v := range n {
		if ov, ok := o[k]; !ok || ov != v {
			out = append(out, k)
		}
	}
	for k := range o {
		if _, ok := n[k]; !ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// withoutConfigEnv drops the variables daemon.ConfigEnv would keep.
func withoutConfigEnv(environ []string) []string {
	cfg := map[string]bool{}
	for _, kv := range daemon.ConfigEnv(environ) {
		cfg[kv] = true
	}
	var out []string
	for _, kv := range environ {
		if !cfg[kv] {
			out = append(out, kv)
		}
	}
	return out
}
