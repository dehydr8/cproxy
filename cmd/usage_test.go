package cmd

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dehydr8/cproxy/internal/config"
)

// cannedSnapshot is a /usage?format=json body shaped like the real wire form.
func cannedSnapshot(now time.Time) string {
	return fmt.Sprintf(`{
  "version": "v1.0.0",
  "upstream": "https://api.anthropic.com",
  "uptime_seconds": 3661,
  "accounts": [
    {"name": "alt", "type": "oauth", "pools": ["default"], "token": "sk-***",
     "requests": 3, "rate_limited": 1, "rejected": 0, "errors": 0,
     "state": "cooling", "cooling_until": %q},
    {"name": "main", "type": "oauth", "pools": ["default"], "token": "sk-***",
     "requests": 142, "rate_limited": 0, "rejected": 0, "errors": 1,
     "state": "ok", "last_used": %q,
     "rate_limits": {"status": "allowed", "requests_limit": 50,
       "requests_remaining": 38, "requests_reset": %q}}
  ],
  "clients": [
    {"name": "amp", "pool": "default", "pinned": "main", "requests": 142,
     "last_seen": %q},
    {"name": "mon", "pool": "default", "pinned": "", "requests": 0}
  ]
}`,
		now.Add(4*time.Minute).UTC().Format(time.RFC3339),
		now.Add(-2*time.Minute).UTC().Format(time.RFC3339),
		now.Add(40*time.Second).UTC().Format(time.RFC3339),
		now.Add(-30*time.Second).UTC().Format(time.RFC3339))
}

func TestRunUsageCmd(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	// amp's token is proxy-only; usage must skip it for mon's usage-scoped one.
	t.Setenv("CPROXY_CLIENT_TOKENS", "amp:cp_proxonly:proxy,mon:cp_usageview:usage")

	var gotAuth, gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		io.WriteString(w, cannedSnapshot(time.Now()))
	}))
	defer ts.Close()
	usagePortFlag = portOf(t, ts.URL)
	usageRawJSON = false

	out, err := runUsageCmdOutput()
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if gotAuth != "Bearer cp_usageview" {
		t.Fatalf("authenticated as %q; want the usage-scoped token", gotAuth)
	}
	if gotPath != "/usage?format=json" {
		t.Fatalf("requested %q", gotPath)
	}
	for _, want := range []string{"main", "142", "cooling", "amp", "req 38/50"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunUsageCmdNoCapableToken(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	// Proxy-only tokens cannot view usage; the command should say so
	// locally instead of making a doomed request.
	t.Setenv("CPROXY_CLIENT_TOKENS", "mon:cp_noview:proxy")

	if _, err := runUsageCmdOutput(); err == nil ||
		!strings.Contains(err.Error(), "no configured client token may view usage") {
		t.Fatalf("err = %v; want the local no-usage-capable-token error", err)
	}
}

func TestRunUsageCmdForbidden(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	// No scopes means capable locally; the server may still disagree
	// (different config there) and its message must surface.
	t.Setenv("CPROXY_CLIENT_TOKENS", "mon:cp_localsaysyes")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"type":"error","error":{"type":"permission_error","message":"cproxy: this token may not view usage (add the \"usage\" scope)"}}`)
	}))
	defer ts.Close()
	usagePortFlag = portOf(t, ts.URL)
	usageRawJSON = false

	if _, err := runUsageCmdOutput(); err == nil ||
		!strings.Contains(err.Error(), "may not view usage") {
		t.Fatalf("err = %v; want the server's permission message", err)
	}
}

func portOf(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	var p int
	if _, err := fmt.Sscanf(u.Port(), "%d", &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// runUsageCmdOutput runs runUsageCmd with stdout captured.
func runUsageCmdOutput() (string, error) {
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	os.Stdout = w
	cmdErr := runUsageCmd(usageCmd, nil)
	w.Close()
	os.Stdout = old
	b, readErr := io.ReadAll(r)
	if cmdErr != nil {
		return "", cmdErr
	}
	if readErr != nil {
		return "", readErr
	}
	return string(b), nil
}
