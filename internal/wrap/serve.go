package wrap

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/dehydr8/cproxy/internal/config"
	"github.com/dehydr8/cproxy/internal/proxy"
)

// DefaultPort is the shared local port the claude wrapper and its serve
// instance agree on (127.0.0.1:7788).
const DefaultPort = 7788

// ServeOptions configures EnsureServe.
type ServeOptions struct {
	Port    int      // shared port; DefaultPort when 0
	Environ []string // inherited by the spawned serve instance
	Token   string   // client token claude will present; verified before use
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
// Before returning, the listener is verified (see Verify) to be a cproxy
// that holds opts.Token, so the token is never handed to whatever else
// happens to own the port, and a serve started with an older configuration
// is reported instead of failing every request with 401.
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
	// A running serve picks up a just-created token within its reload poll
	// (~2s), so a serve that does not know the token yet gets a moment.
	deadline := time.Now().Add(verifyGrace)
	for {
		err := Verify(addr, opts.Token)
		if err == nil {
			break
		}
		var stale *StaleServeError
		if !errors.As(err, &stale) || time.Now().After(deadline) {
			return spawned, err
		}
		time.Sleep(200 * time.Millisecond)
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

// Verify checks that the listener on addr is a cproxy holding token, without
// sending the token itself (see proxy.WhoamiPath). Its errors say what is
// wrong and how to fix it: something that is not cproxy owns the port, an
// older cproxy is still serving, or a cproxy started with a different
// configuration does not know this token.
func Verify(addr, token string) error {
	nb := make([]byte, 16)
	if _, err := rand.Read(nb); err != nil {
		return err
	}
	nonce := hex.EncodeToString(nb)
	q := url.Values{"client": {proxy.TokenHash(token)}, "nonce": {nonce}}
	u := "http://" + addr + proxy.WhoamiPath + "?" + q.Encode()

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		return fmt.Errorf("could not check the listener on %s: %w", addr, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	var who proxy.WhoamiResponse
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &who) != nil || who.Cproxy == "" {
		if strings.Contains(string(body), `"cproxy: `) {
			// Pre-whoami cproxy builds answer with their own auth error.
			return fmt.Errorf("an older cproxy serve is running on %s; stop it (e.g. pkill -f 'cproxy serve') and the next launch starts a current one", addr)
		}
		return fmt.Errorf("something other than cproxy is listening on %s; cproxy will not send your client token to it. Free the port or pick another with %s / --port", addr, config.EnvPortName)
	}
	if !hmac.Equal([]byte(who.Proof), []byte(proxy.WhoamiProof(token, nonce))) {
		return &StaleServeError{Addr: addr, PID: who.PID, Version: who.Cproxy}
	}
	return nil
}

// verifyGrace is how long EnsureServe waits for a running serve to reload a
// token it does not know yet (serve polls its credential files every 2s).
var verifyGrace = 3 * time.Second

// StaleServeError reports a cproxy serve that does not know the client
// token, i.e. one started with a different configuration.
type StaleServeError struct {
	Addr    string
	PID     int
	Version string
}

func (e *StaleServeError) Error() string {
	return fmt.Sprintf("the cproxy serve on %s (pid %d, %s) does not know this client token: it was started with a different configuration, and environment variables are only read when it starts. "+
		"Restart it with `kill %d` (the next launch starts a fresh one), or use another %s", e.Addr, e.PID, e.Version, e.PID, config.EnvPortName)
}
