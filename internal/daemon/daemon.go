// Package daemon is the local cproxy daemon's control plane: one daemon per
// user (an exclusive lock in ~/.cproxy), reached by every cproxy command over
// a private unix socket, the way flyctl talks to its agent. Claude itself
// still talks to the daemon's 127.0.0.1 TCP port; the socket is for cproxy:
// status, configuration sync, client registration, usage and hand-over.
package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dehydr8/cproxy/internal/config"
)

// ErrRunning means another daemon holds the lock.
var ErrRunning = errors.New("a cproxy daemon is already running")

// ErrUnsupported means the running daemon predates the request (an older
// cproxy without that control endpoint).
var ErrUnsupported = errors.New("the running cproxy daemon does not support this request")

// SocketPath is the daemon's control socket. Unix socket paths are limited
// to about 104 bytes, so a deep CPROXY_HOME falls back to a per-user path in
// the temp directory.
func SocketPath() string {
	p := filepath.Join(config.Home(), "cproxy.sock")
	if len(p) < 100 {
		return p
	}
	sum := sha256.Sum256([]byte(config.Home()))
	return filepath.Join(os.TempDir(), "cproxy-"+strconv.Itoa(os.Getuid())+"-"+hex.EncodeToString(sum[:4])+".sock")
}

// LockPath is held exclusively by the running daemon.
func LockPath() string { return filepath.Join(config.Home(), "daemon.lock") }

// LogPath is where a spawned daemon logs.
func LogPath() string { return filepath.Join(config.Home(), "daemon.log") }

// Status is what the daemon reports about itself.
type Status struct {
	PID     int       `json:"pid"`
	Version string    `json:"version"`
	Addr    string    `json:"addr"` // 127.0.0.1:port that claude talks to
	Started time.Time `json:"started"`
	EnvHash string    `json:"env_hash"` // ConfigEnv fingerprint it runs with
}

// ConfigEnv is the part of an environment that configures the daemon: the
// CPROXY_* variables, minus those that only steer the calling command.
func ConfigEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(k, "CPROXY_") {
			continue
		}
		switch k {
		case config.EnvHome, config.EnvPortName, config.EnvAuthTokenName:
			continue
		}
		out = append(out, kv)
	}
	slices.Sort(out)
	return out
}

// EnvHash fingerprints ConfigEnv(environ).
func EnvHash(environ []string) string {
	sum := sha256.Sum256([]byte(strings.Join(ConfigEnv(environ), "\n")))
	return hex.EncodeToString(sum[:8])
}

// Handler serves the control protocol; the daemon fills in the funcs.
type Handler struct {
	Status   func() Status
	SyncEnv  func(environ []string) (reloaded bool, err error)
	Register func(name, pool string) (token string, err error)
	Usage    http.Handler
	Shutdown func()
	Restart  func() // hand over to a successor started with the daemon's own environment
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/status":
		writeJSON(w, h.Status())
	case r.Method == http.MethodPost && r.URL.Path == "/env":
		var req struct {
			Environ []string `json:"environ"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reloaded, err := h.SyncEnv(ConfigEnv(req.Environ))
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		writeJSON(w, map[string]bool{"reloaded": reloaded})
	case r.Method == http.MethodPost && r.URL.Path == "/clients":
		var req struct {
			Name string `json:"name"`
			Pool string `json:"pool"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		tok, err := h.Register(req.Name, req.Pool)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		writeJSON(w, map[string]string{"token": tok})
	case r.Method == http.MethodGet && r.URL.Path == "/usage":
		h.Usage.ServeHTTP(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/shutdown":
		writeJSON(w, map[string]bool{"ok": true})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		go h.Shutdown()
	case r.Method == http.MethodPost && r.URL.Path == "/restart":
		writeJSON(w, map[string]bool{"ok": true})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		go h.Restart()
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// Client talks to the daemon over its socket.
type Client struct {
	hc *http.Client
}

// Dial connects to the running daemon, or fails when none answers.
func Dial() (*Client, Status, error) {
	sock := SocketPath()
	c := &Client{hc: &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}},
	}}
	var st Status
	if err := c.do(http.MethodGet, "/status", nil, &st); err != nil {
		return nil, Status{}, err
	}
	return c, st, nil
}

func (c *Client) do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://cproxy"+path, body)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("cproxy daemon: %s", strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// SyncEnv hands the daemon this environment's configuration; it reloads when
// that differs from what it runs with (the last command to start wins).
func (c *Client) SyncEnv(environ []string) (bool, error) {
	var out struct {
		Reloaded bool `json:"reloaded"`
	}
	err := c.do(http.MethodPost, "/env", map[string][]string{"environ": ConfigEnv(environ)}, &out)
	return out.Reloaded, err
}

// Register returns the token of the named client (created on first use),
// routed to pool ("" for the daemon's own pool).
func (c *Client) Register(name, pool string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	err := c.do(http.MethodPost, "/clients", map[string]string{"name": name, "pool": pool}, &out)
	return out.Token, err
}

// Usage returns the daemon's usage snapshot JSON (as /usage?format=json).
func (c *Client) Usage() ([]byte, error) {
	resp, err := c.hc.Get("http://cproxy/usage")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// Shutdown asks the daemon to hand over: it stops listening at once and
// finishes the requests it is serving in the background.
func (c *Client) Shutdown() error { return c.do(http.MethodPost, "/shutdown", nil, nil) }

// Restart asks the daemon to hand over to a fresh daemon of the cproxy
// binary now installed, started with the daemon's own environment (so
// configuration only a launch handed it survives), then finish its streams.
func (c *Client) Restart() error { return c.do(http.MethodPost, "/restart", nil, nil) }

// Newer reports whether version a is a newer release than b (vX.Y.Z;
// development builds compare as neither newer nor older).
func Newer(a, b string) bool {
	pa, oka := semver(a)
	pb, okb := semver(b)
	if !oka || !okb {
		return false
	}
	return slices.Compare(pa, pb) > 0
}

func semver(v string) ([]int, bool) {
	v, ok := strings.CutPrefix(strings.TrimSpace(v), "v")
	if !ok {
		return nil, false
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return nil, false
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}
