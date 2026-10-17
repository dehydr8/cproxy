package cmd

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Claude Code's public OAuth client, as used by `claude setup-token`. The
// manual-callback flow lets cproxy mint a long-lived token in a terminal
// without owning a port or opening a browser.
const (
	oauthAuthorizeURL = "https://claude.com/cai/oauth/authorize"
	oauthTokenURL     = "https://platform.claude.com/v1/oauth/token"
	oauthClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	oauthRedirectURI  = "https://platform.claude.com/oauth/code/callback"
)

// oauthTokenEndpoint is swapped in tests for a fake token server.
var oauthTokenEndpoint = oauthTokenURL

var (
	setupScopes    string
	setupExpiresIn int64
)

var setupTokenCmd = &cobra.Command{
	Use:   "setup-token [--scopes SCOPES] [--expires-in SECONDS]",
	Short: "Mint a long-lived Anthropic OAuth token via browser sign-in",
	Long: `Runs the same browser sign-in flow as claude setup-token: cproxy prints a
URL, you open it and approve with the account you want to pool, then paste
back the code#state shown after approval. The minted token is printed on
stdout; nothing is written automatically and no Claude files are touched.

The default scopes add user:profile to claude setup-token's user:inference,
and the default lifetime is one year.

Pipe the result straight into an account:

	cproxy setup-token | cproxy accounts add cvp2

or capture it and use ` + "`cproxy accounts add NAME`" + ` interactively.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		scopes := strings.Fields(setupScopes)
		if len(scopes) == 0 {
			return fmt.Errorf("--scopes cannot be empty")
		}
		if setupExpiresIn <= 0 {
			return fmt.Errorf("--expires-in must be a positive number of seconds")
		}
		return runSetupToken(os.Stdin, os.Stdout, os.Stderr, scopes, setupExpiresIn)
	},
}

// runSetupToken mints a token with a fresh PKCE challenge and state.
func runSetupToken(in io.Reader, out, errOut io.Writer, scopes []string, expiresIn int64) error {
	verifier, challenge, state := newOAuthChallenge()
	authURL := buildAuthorizeURL(scopes, challenge, state)
	return setupTokenExchange(in, out, errOut, authURL, verifier, state, scopes, expiresIn)
}

// newOAuthChallenge generates a PKCE verifier (43 unreserved chars), its
// S256 challenge and a random state.
func newOAuthChallenge() (verifier, challenge, state string) {
	vb := make([]byte, 32)
	rand.Read(vb)
	verifier = base64.RawURLEncoding.EncodeToString(vb)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	sb := make([]byte, 16)
	rand.Read(sb)
	state = hex.EncodeToString(sb)
	return verifier, challenge, state
}

// buildAuthorizeURL builds the browser sign-in URL.
func buildAuthorizeURL(scopes []string, challenge, state string) string {
	q := url.Values{
		"code":                  {"true"},
		"client_id":             {oauthClientID},
		"response_type":         {"code"},
		"redirect_uri":          {oauthRedirectURI},
		"scope":                 {strings.Join(scopes, " ")},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	return oauthAuthorizeURL + "?" + q.Encode()
}

// setupTokenExchange performs the interactive half of the flow: shows the
// sign-in URL, reads the pasted code#state, exchanges it and prints the raw
// token on out (diagnostics on errOut).
func setupTokenExchange(in io.Reader, out, errOut io.Writer, authURL, verifier, state string, scopes []string, expiresIn int64) error {
	fmt.Fprintf(errOut, "Open this URL, sign in with the account to mint a token for, and approve:\n\n%s\n\n", authURL)
	fmt.Fprint(errOut, "Paste the code shown after approval (code#state): ")

	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return fmt.Errorf("read code from stdin: %w", err)
	}
	code, pastedState, _ := strings.Cut(strings.TrimSpace(line), "#")
	if code == "" || pastedState != state {
		return fmt.Errorf("that is not the code for this sign-in (expected code#%s)", state)
	}

	body, _ := json.Marshal(map[string]any{
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  oauthRedirectURI,
		"client_id":     oauthClientID,
		"code_verifier": verifier,
		"state":         state,
		"expires_in":    expiresIn,
	})
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Post(
		oauthTokenEndpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("token exchange failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token exchange failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(respBody, &tok); err != nil {
		return fmt.Errorf("token exchange response: %w", err)
	}
	if tok.AccessToken == "" {
		return fmt.Errorf("token exchange returned no access token")
	}

	granted := tok.Scope
	if granted == "" {
		granted = strings.Join(scopes, " ")
	}
	lifetime := "not reported"
	if tok.ExpiresIn > 0 {
		lifetime = fmt.Sprintf("%.1f days", float64(tok.ExpiresIn)/86400)
	}
	refresh := "no"
	if tok.RefreshToken != "" {
		refresh = "yes"
	}
	fmt.Fprintf(errOut, "\nGranted scopes: %s\nLifetime: %s\nRefresh token returned: %s\nRegister it with: cproxy accounts add NAME (or pipe: cproxy setup-token | cproxy accounts add NAME)\n",
		granted, lifetime, refresh)
	fmt.Fprintln(out, tok.AccessToken)
	return nil
}

func init() {
	setupTokenCmd.Flags().StringVar(&setupScopes, "scopes", "user:inference user:profile",
		"space-separated OAuth scopes to request")
	setupTokenCmd.Flags().Int64Var(&setupExpiresIn, "expires-in", 31536000,
		"requested token lifetime in seconds (default: one year)")
}
