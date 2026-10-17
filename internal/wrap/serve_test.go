package wrap

import (
	"net"
	"os"
	"strconv"
	"testing"

	"github.com/dehydr8/cproxy/internal/config"
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
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					os.Exit(0)
				}
				c.Close()
			}
		}()
		select {}
	}
	os.Exit(m.Run())
}

func TestEnsureServeAlreadyListening(t *testing.T) {
	t.Setenv("AMP_THREAD_ID", "") // never register Amp portals from tests
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	cmd, err := EnsureServe(ServeOptions{Port: port})
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
		"CPROXY_WRAP_TEST_PORT="+strconv.Itoa(port))
	cmd, err := EnsureServe(ServeOptions{Port: port, Environ: environ})
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
