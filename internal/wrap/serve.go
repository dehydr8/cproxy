package wrap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/dehydr8/cproxy/internal/config"
)

// DefaultPort is the shared local port the claude wrapper and its serve
// instance agree on (127.0.0.1:7788).
const DefaultPort = 7788

// ServeOptions configures EnsureServe.
type ServeOptions struct {
	Port    int      // shared port; DefaultPort when 0
	Environ []string // inherited by the spawned serve instance
}

// EnsureServe makes sure a cproxy serve instance is listening on
// 127.0.0.1:Port, spawning one when nothing is. It returns the spawned
// process (nil when one was already listening) so callers that want to
// tear it down can.
//
// The spawned serve is detached from this process and inherits Environ, so
// environment-based credentials (CPROXY_TOKENS, CPROXY_CLIENT_TOKENS, …)
// reach it without any service, profile, or login-shell wiring. It keeps
// serving after claude exits; if it dies (machine reboot, orb update), the
// next shim launch starts a fresh one.
//
// In an Amp orb (AMP_THREAD_ID set and the amp CLI on PATH) the usage
// dashboard is also exposed as a portal, best-effort.
func EnsureServe(opts ServeOptions) (*exec.Cmd, error) {
	if opts.Port == 0 {
		opts.Port = DefaultPort
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.Port))

	var spawned *exec.Cmd
	if !listening(addr) {
		self, err := os.Executable()
		if err != nil {
			return nil, err
		}
		spawned, err = spawnServe(self, addr, opts.Environ)
		if err != nil {
			return nil, err
		}
		if err := waitListening(spawned, addr, 10*time.Second); err != nil {
			return spawned, fmt.Errorf("cproxy serve did not come up on %s (log: %s): %w", addr, ServeLogPath(), err)
		}
	}
	exposeUsagePortal(addr)
	return spawned, nil
}

// spawnServe starts a detached `cproxy serve --addr addr` that inherits
// environ and logs to ~/.cproxy/serve.log.
func spawnServe(self, addr string, environ []string) (*exec.Cmd, error) {
	if err := os.MkdirAll(config.Home(), 0o700); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(ServeLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open serve log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(self, "serve", "--addr", addr)
	cmd.Env = environ
	cmd.Stdout, cmd.Stderr = logFile, logFile
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start cproxy serve: %w", err)
	}
	return cmd, nil
}

// waitListening polls addr until something accepts, the spawned serve
// exits, or the deadline passes.
func waitListening(cmd *exec.Cmd, addr string, d time.Duration) error {
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.Now().Add(d)
	for {
		if listening(addr) {
			return nil
		}
		select {
		case err := <-exited:
			if err != nil {
				return err
			}
			return errors.New("serve exited unexpectedly")
		default:
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for the port")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// listening reports whether anything accepts connections on addr.
func listening(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// exposeUsagePortal registers the /usage dashboard as an Amp portal when
// running inside an orb. Best-effort: outside Amp nothing happens.
func exposeUsagePortal(addr string) {
	if os.Getenv("AMP_THREAD_ID") == "" {
		return
	}
	amp, err := exec.LookPath("amp")
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, amp, "orb", "portal", "http://"+addr+"/usage", "--title", "cproxy usage")
	_ = cmd.Run()
}
