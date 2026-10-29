package wrap

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
