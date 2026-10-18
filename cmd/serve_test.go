package cmd

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dehydr8/cproxy/internal/config"
)

func TestBuildServeStateClientTokenScopes(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	t.Setenv(config.EnvTokens, "main:sk-ant-oat01-x")
	old := serveClientTokens
	t.Cleanup(func() { serveClientTokens = old })

	serveClientTokens = "mon:cp_mon:usage,alice:cp_alice"
	st, _, err := buildServeState()
	if err != nil {
		t.Fatal(err)
	}
	if mon := st.Runtime.ClientAuth["cp_mon"]; mon.HasScope(config.ScopeProxy) || !mon.HasScope(config.ScopeUsage) {
		t.Fatalf("usage-only --client-tokens entry = %+v; must not get proxy access", mon)
	}
	if alice := st.Runtime.ClientAuth["cp_alice"]; !alice.HasScope(config.ScopeProxy) || !alice.HasScope(config.ScopeUsage) {
		t.Fatalf("unscoped entry = %+v; want full access", alice)
	}

	serveClientTokens = "mon:cp_mon:admin"
	if _, _, err := buildServeState(); err == nil {
		t.Fatal("unknown scope in --client-tokens was accepted")
	}
}

func TestWatchReloadsOnFileChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var reloads atomic.Int32
	hup := make(chan os.Signal)
	done := make(chan struct{})
	go func() {
		watchReloads(hup, path, 5*time.Millisecond, func() { reloads.Add(1) })
		close(done)
	}()
	waitFor := func(n int32, what string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for reloads.Load() < n {
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d reloads, want %d", what, reloads.Load(), n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	time.Sleep(30 * time.Millisecond)
	if n := reloads.Load(); n != 0 {
		t.Fatalf("reloaded %d times without a change", n)
	}
	if err := os.WriteFile(path, []byte(`{"accounts":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(1, "after edit")
	hup <- syscall.SIGHUP
	waitFor(2, "after SIGHUP")
	time.Sleep(30 * time.Millisecond)
	if n := reloads.Load(); n != 2 {
		t.Fatalf("%d reloads; a single edit must reload once", n)
	}
	close(hup)
	<-done
}
