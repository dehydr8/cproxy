package wrap

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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
	// No ambient configuration in tests: the developer's own pool tokens
	// must never reach a test or its output.
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "CPROXY_") || strings.HasPrefix(k, "ANTHROPIC_") || k == "CLAUDE_CODE_OAUTH_TOKEN" {
			os.Unsetenv(k)
		}
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

func TestVerifyWaitsForReload(t *testing.T) {
	// A running serve learns a just-created local token on its next reload.
	var h atomic.Value
	h.Store(cproxyKnowing("cp_old"))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.Load().(http.Handler).ServeHTTP(w, r)
	}))
	defer ts.Close()
	time.AfterFunc(500*time.Millisecond, func() { h.Store(cproxyKnowing("cp_new")) })

	if err := VerifyWithGrace(ts.Listener.Addr().String(), "cp_new"); err != nil {
		t.Fatalf("VerifyWithGrace did not wait for the reload: %v", err)
	}
}

func TestVerifyRefusesWrongListener(t *testing.T) {
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
		err := VerifyWithGrace(tc.ts.Listener.Addr().String(), token)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v; want it to mention %q", tc.name, err, tc.want)
		}
	}
	for _, s := range seen {
		if strings.Contains(s, token) {
			t.Fatalf("the client token reached a foreign listener: %s", s)
		}
	}
}
