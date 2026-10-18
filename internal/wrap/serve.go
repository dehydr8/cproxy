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
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dehydr8/cproxy/internal/config"
	"github.com/dehydr8/cproxy/internal/proxy"
)

// DefaultPort is the local port the daemon prefers for claude
// (127.0.0.1:7788); it takes any free port when that one is busy.
const DefaultPort = 7788

// VerifyWithGrace is Verify, giving a daemon that does not know token yet a
// moment to reload it (it polls its credential files every 2s).
func VerifyWithGrace(addr, token string) error {
	deadline := time.Now().Add(verifyGrace)
	for {
		err := Verify(addr, token)
		var stale *StaleServeError
		if err == nil || !errors.As(err, &stale) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// InAmpOrb reports whether the wrapper runs inside an Amp orb that can
// register portals (AMP_THREAD_ID set and the amp CLI on PATH).
func InAmpOrb() bool {
	if os.Getenv("AMP_THREAD_ID") == "" {
		return false
	}
	_, err := exec.LookPath("amp")
	return err == nil
}

// ExposeUsagePortal registers the /usage dashboard as an Amp portal when
// running inside an orb, with token (usage-only) in its URL so the page
// opens straight away; portals are private to the thread's owner unless
// they choose otherwise. Best-effort: outside Amp nothing happens.
func ExposeUsagePortal(addr, token string) {
	if !InAmpOrb() {
		return
	}
	amp, _ := exec.LookPath("amp")
	target := "http://" + addr + "/usage"
	if token != "" {
		target += "?token=" + url.QueryEscape(token)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, amp, "orb", "portal", target, "--name", "cproxy-usage",
		"--title", "cproxy usage", "--description", "Live pool usage: accounts, limits and clients")
	cmd.Dir = portalDir()
	_ = cmd.Run()
}

// portalDir is where `amp orb portal` runs, and so where it writes the
// portal manifest (.amp/portals/): the orb's workspace, which Amp reads and
// git-excludes, rather than whatever directory claude was launched in.
func portalDir() string {
	if d := os.Getenv("AMP_WORKING_DIRECTORY"); d != "" {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
	}
	return config.Home()
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
