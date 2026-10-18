package wrap

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dehydr8/cproxy/internal/config"
	"github.com/dehydr8/cproxy/internal/proxy"
)

// TestMain lets this test binary double as the "serve" process spawned by
// EnsureServe: with CPROXY_WRAP_TEST_SERVE=1 it just listens on
// CPROXY_WRAP_TEST_PORT forever, which is all EnsureServe needs to observe.
func TestMain(m *testing.M) {
	if os.Getenv("CPROXY_WRAP_TEST_SERVE") == "1" {
		addr := net.JoinHostPort("127.0.0.1", os.Getenv("CPROXY_WRAP_TEST_PORT"))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			os.Exit(1)
		}
		http.Serve(ln, cproxyKnowing(os.Getenv("CPROXY_WRAP_TEST_TOKEN")))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// cproxyKnowing is a real cproxy handler configured with one client token.
func cproxyKnowing(token string) http.Handler {
	u, _ := url.Parse("http://127.0.0.1:1")
	rt := &config.Runtime{ClientAuth: map[string]config.ClientAuth{token: {Name: "t"}}}
	return proxy.New(proxy.Options{Upstream: u, State: &proxy.State{Runtime: rt},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

func TestEnsureServeAlreadyListening(t *testing.T) {
	t.Setenv("AMP_THREAD_ID", "") // never register Amp portals from tests
	ts := httptest.NewServer(cproxyKnowing("cp_mine"))
	defer ts.Close()
	port := ts.Listener.Addr().(*net.TCPAddr).Port

	cmd, err := EnsureServe(ServeOptions{Port: port, Token: "cp_mine"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd != nil {
		t.Fatal("spawned a serve although one was already listening")
	}
}

func TestEnsureServeSpawns(t *testing.T) {
	t.Setenv("AMP_THREAD_ID", "")
	t.Setenv(config.EnvHome, t.TempDir())

	// Pick a free port, then give it up for the spawned serve to take.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	environ := append(os.Environ(),
		"CPROXY_WRAP_TEST_SERVE=1",
		"CPROXY_WRAP_TEST_PORT="+strconv.Itoa(port),
		"CPROXY_WRAP_TEST_TOKEN=cp_mine")
	cmd, err := EnsureServe(ServeOptions{Port: port, Environ: environ, Token: "cp_mine"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd == nil || cmd.Process == nil {
		t.Fatal("no serve process spawned")
	}
	defer cmd.Process.Kill()

	if !listening(net.JoinHostPort("127.0.0.1", strconv.Itoa(port))) {
		t.Fatal("spawned serve is not listening")
	}
}

func TestEnsureServeWaitsForReload(t *testing.T) {
	t.Setenv("AMP_THREAD_ID", "")
	// A running serve learns a just-created local token on its next reload.
	var h atomic.Value
	h.Store(cproxyKnowing("cp_old"))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.Load().(http.Handler).ServeHTTP(w, r)
	}))
	defer ts.Close()
	time.AfterFunc(500*time.Millisecond, func() { h.Store(cproxyKnowing("cp_new")) })

	port := ts.Listener.Addr().(*net.TCPAddr).Port
	if _, err := EnsureServe(ServeOptions{Port: port, Token: "cp_new"}); err != nil {
		t.Fatalf("EnsureServe did not wait for the reload: %v", err)
	}
}

func TestEnsureServeRefusesWrongListener(t *testing.T) {
	t.Setenv("AMP_THREAD_ID", "")
	old := verifyGrace
	verifyGrace = 0
	t.Cleanup(func() { verifyGrace = old })
	const token = "cp_secret_token_value"

	var seen []string
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.String(), fmt.Sprint(r.Header))
		http.NotFound(w, r)
	}))
	defer foreign.Close()
	older := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"cproxy: invalid or missing client token"}}`)
	}))
	defer older.Close()
	stale := httptest.NewServer(cproxyKnowing("cp_someone_else"))
	defer stale.Close()

	for _, tc := range []struct {
		name string
		ts   *httptest.Server
		want string
	}{
		{"not cproxy", foreign, "something other than cproxy"},
		{"pre-whoami cproxy", older, "older cproxy serve"},
		{"different config", stale, fmt.Sprintf("kill %d", os.Getpid())},
	} {
		port := tc.ts.Listener.Addr().(*net.TCPAddr).Port
		cmd, err := EnsureServe(ServeOptions{Port: port, Token: token})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v; want it to mention %q", tc.name, err, tc.want)
		}
		if cmd != nil {
			t.Fatalf("%s: spawned a serve over a live listener", tc.name)
		}
	}
	for _, s := range seen {
		if strings.Contains(s, token) {
			t.Fatalf("the client token reached a foreign listener: %s", s)
		}
	}
}
