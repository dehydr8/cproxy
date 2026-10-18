package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dehydr8/cproxy/internal/daemon"
	"github.com/dehydr8/cproxy/internal/proxy"
	"github.com/dehydr8/cproxy/internal/wrap"
)

// daemonEnv sets up a home and a fake upstream for a daemon under test and
// returns the environment launches would hand it. The daemon is stopped
// when the test ends.
func daemonEnv(t *testing.T, upstream http.HandlerFunc, extra ...string) []string {
	t.Helper()
	home, err := os.MkdirTemp("", "cpd") // short: unix socket paths are length-limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	t.Setenv("CPROXY_HOME", home) // this process looks for the same socket
	env := append(os.Environ(),
		"CPROXY_HOME="+home,
		"CPROXY_UPSTREAM="+up.URL,
		"CPROXY_PORT="+strconv.Itoa(port),
		"CPROXY_TOKEN_A=sk-ant-oat01-a",
	)
	env = append(env, extra...)
	t.Cleanup(func() {
		if c, _, err := daemon.Dial(); err == nil {
			c.Shutdown()
		}
	})
	return env
}

func echoAuth(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"auth":%q}`, r.Header.Get("Authorization"))
}

func TestDaemonSharedAcrossLaunches(t *testing.T) {
	env := daemonEnv(t, echoAuth)

	// Many launches at once end up on one daemon.
	var wg sync.WaitGroup
	pids := make([]int, 6)
	errs := make([]error, 6)
	for i := range pids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, st, err := wrap.EnsureDaemon(env)
			pids[i], errs[i] = st.PID, err
		}()
	}
	wg.Wait()
	for i := range pids {
		if errs[i] != nil {
			t.Fatalf("launch %d: %v", i, errs[i])
		}
		if pids[i] != pids[0] {
			t.Fatalf("launches got daemons %v; want one", pids)
		}
	}

	// A launch with a changed environment reconfigures it in place.
	c, st, err := wrap.EnsureDaemon(append(env, "CPROXY_TOKEN_B=sk-ant-oat01-b"))
	if err != nil {
		t.Fatal(err)
	}
	if st.PID != pids[0] {
		t.Fatalf("a changed environment restarted the daemon (%d -> %d)", pids[0], st.PID)
	}
	accounts := func() int {
		t.Helper()
		body, err := c.Usage()
		if err != nil {
			t.Fatal(err)
		}
		var snap proxy.UsageSnapshot
		if err := json.Unmarshal(body, &snap); err != nil {
			t.Fatal(err)
		}
		return len(snap.Accounts)
	}
	if n := accounts(); n != 2 {
		t.Fatalf("after the env sync the daemon has %d accounts, want 2", n)
	}

	// A shell without any CPROXY_* configuration (only where to find the
	// daemon) and a read-only command never take the accounts away.
	var bare []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "CPROXY_TOKEN_") {
			bare = append(bare, kv)
		}
	}
	if _, _, err := wrap.ConnectDaemon(bare[:len(bare):len(bare)]); err != nil {
		t.Fatal(err)
	}
	var noConfig []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CPROXY_") || strings.HasPrefix(kv, "CPROXY_HOME=") {
			noConfig = append(noConfig, kv)
		}
	}
	if _, _, err := wrap.EnsureDaemon(noConfig); err != nil {
		t.Fatal(err)
	}
	if n := accounts(); n != 2 {
		t.Fatalf("a shell without the variables left the daemon %d accounts, want 2", n)
	}
}

func TestDaemonPoolClients(t *testing.T) {
	env := daemonEnv(t, echoAuth, "CPROXY_POOL_TOKEN_CVP_X=sk-ant-oat01-cvpx")
	c, st, err := wrap.EnsureDaemon(env)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.Register("claude-local@cvp", "cvp")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := c.Register("claude-local@cvp", "cvp"); again != tok {
		t.Fatal("the same pool got a second identity")
	}
	req, _ := http.NewRequest("POST", "http://"+st.Addr+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "sk-ant-oat01-cvpx") {
		t.Fatalf("pool cvp request went upstream as %s", b)
	}
	if _, err := c.Register("x", "nope"); err == nil {
		t.Fatal("registered a client for an unknown pool")
	}
}

func TestDaemonHandOverKeepsStreams(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	env := daemonEnv(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-release
		io.WriteString(w, "data: two\n\n")
	})
	c, st, err := wrap.EnsureDaemon(env)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := c.Register("claude-local", "")

	got := make(chan string, 1)
	go func() {
		req, _ := http.NewRequest("POST", "http://"+st.Addr+"/v1/messages", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			got <- err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		got <- string(b)
	}()
	<-started

	// Hand over mid-stream: a new daemon takes the port and the socket at
	// once while the old one finishes the stream it is serving.
	if err := c.Shutdown(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var next daemon.Status
	for {
		_, next, err = wrap.EnsureDaemon(env)
		if err == nil && next.PID != st.PID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no successor daemon: %v (pid %d)", err, next.PID)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if next.Addr != st.Addr {
		t.Fatalf("successor on %s, want the same address %s", next.Addr, st.Addr)
	}
	close(release)
	if body := <-got; !strings.Contains(body, "data: two") {
		t.Fatalf("the stream was cut by the hand-over: %q", body)
	}
}
