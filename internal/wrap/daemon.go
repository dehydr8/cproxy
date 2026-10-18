package wrap

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/dehydr8/cproxy/internal/config"
	"github.com/dehydr8/cproxy/internal/daemon"
	"github.com/dehydr8/cproxy/internal/version"
)

// daemonStartTimeout bounds how long a launch waits for a daemon to come up.
var daemonStartTimeout = 10 * time.Second

// EnsureDaemon returns a client of the per-user daemon and its status. It
// starts the daemon when none runs, and when this cproxy is a newer release
// than the running daemon it asks that one to hand over (it stops listening
// at once and finishes its streams) and starts a fresh one. Finally it
// hands the daemon environ's CPROXY_* configuration; the daemon reloads when
// that differs from what it runs with, so a changed environment never needs
// a manual restart. An environment with no CPROXY_* configuration at all
// (a shell without the variables) never replaces the daemon's.
func EnsureDaemon(environ []string) (*daemon.Client, daemon.Status, error) {
	return ensureDaemon(environ, true)
}

// ConnectDaemon is EnsureDaemon for read-only commands (usage, dashboard):
// it starts the daemon when none runs but never reconfigures a running one,
// so looking at usage from another shell cannot change what claude uses.
func ConnectDaemon(environ []string) (*daemon.Client, daemon.Status, error) {
	return ensureDaemon(environ, false)
}

func ensureDaemon(environ []string, sync bool) (*daemon.Client, daemon.Status, error) {
	c, st, err := daemon.Dial()
	if err == nil && daemon.Newer(version.String(), st.Version) {
		if err := c.Shutdown(); err != nil {
			return nil, daemon.Status{}, fmt.Errorf("ask the cproxy %s daemon to hand over: %w", st.Version, err)
		}
		waitGone(5 * time.Second)
		err = errors.New("handed over")
	}
	if err != nil {
		if err := spawnDaemon(environ); err != nil {
			return nil, daemon.Status{}, err
		}
		if c, st, err = waitDaemon(daemonStartTimeout); err != nil {
			return nil, daemon.Status{}, fmt.Errorf("the cproxy daemon did not start (log: %s): %w", daemon.LogPath(), err)
		}
	}
	if !sync || len(daemon.ConfigEnv(environ)) == 0 {
		return c, st, nil
	}
	if _, err := c.SyncEnv(environ); err != nil {
		return nil, st, fmt.Errorf("configure the cproxy daemon: %w", err)
	}
	return c, st, nil
}

// spawnDaemon starts a detached `cproxy daemon` that inherits environ and
// logs to daemon.LogPath(). Concurrent launches may each start one; the
// lock lets one win and the others exit at once.
func spawnDaemon(environ []string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(config.Home(), 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(daemon.LogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open daemon log: %w", err)
	}
	defer logFile.Close()
	cmd := exec.Command(self, "daemon")
	cmd.Env = environ
	cmd.Stdout, cmd.Stderr = logFile, logFile
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the cproxy daemon: %w", err)
	}
	go cmd.Wait() // reap a daemon that lost the race and exited
	return nil
}

func waitDaemon(d time.Duration) (*daemon.Client, daemon.Status, error) {
	deadline := time.Now().Add(d)
	for {
		c, st, err := daemon.Dial()
		if err == nil {
			return c, st, nil
		}
		if time.Now().After(deadline) {
			return nil, daemon.Status{}, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitGone waits for a daemon that is handing over to release its socket.
func waitGone(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, _, err := daemon.Dial(); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
