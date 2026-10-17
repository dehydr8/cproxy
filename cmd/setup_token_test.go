package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestBuildAuthorizeURL(t *testing.T) {
	got := buildAuthorizeURL([]string{"user:inference", "user:profile"}, "ch4ll", "st4t3")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "claude.com" || u.Path != "/cai/oauth/authorize" {
		t.Fatalf("authorize URL: %s", got)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"code":                  "true",
		"client_id":             oauthClientID,
		"response_type":         "code",
		"redirect_uri":          oauthRedirectURI,
		"scope":                 "user:inference user:profile",
		"code_challenge":        "ch4ll",
		"code_challenge_method": "S256",
		"state":                 "st4t3",
	} {
		if q.Get(k) != want {
			t.Fatalf("authorize URL %s = %q, want %q", k, q.Get(k), want)
		}
	}
}

func TestNewOAuthChallenge(t *testing.T) {
	verifier, challenge, state := newOAuthChallenge()
	if len(verifier) != 43 {
		t.Fatalf("verifier length = %d, want 43", len(verifier))
	}
	sum := sha256.Sum256([]byte(verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); challenge != want {
		t.Fatalf("challenge = %q, want S256(verifier) = %q", challenge, want)
	}
	if len(state) != 32 {
		t.Fatalf("state length = %d, want 32", len(state))
	}
	v2, _, s2 := newOAuthChallenge()
	if v2 == verifier || s2 == state {
		t.Fatal("challenge is not random")
	}
}

func TestSetupTokenExchange(t *testing.T) {
	var (
		mu       sync.Mutex
		gotBody  map[string]any
		srv      *httptest.Server
		failWith string
	)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/oauth/token" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if failWith != "" {
			http.Error(w, `{"error":"invalid_grant"}`, 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "sk-ant-oat01-new",
			"refresh_token": "rt_123",
			"expires_in":    31536000,
			"scope":         "user:inference user:profile",
		})
	}))
	t.Cleanup(srv.Close)

	old := oauthTokenEndpoint
	oauthTokenEndpoint = srv.URL + "/v1/oauth/token"
	t.Cleanup(func() { oauthTokenEndpoint = old })

	authURL := buildAuthorizeURL([]string{"user:inference", "user:profile"}, "ch4ll", "st4t3")
	run := func(input string) (string, string, error) {
		var out, errOut bytes.Buffer
		err := setupTokenExchange(strings.NewReader(input), &out, &errOut, authURL, "ver1f13r", "st4t3", []string{"user:inference", "user:profile"}, 31536000)
		return out.String(), errOut.String(), err
	}

	stdout, stderr, err := run("goodcode#st4t3\n")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "sk-ant-oat01-new\n" {
		t.Fatalf("stdout = %q, want only the raw token", stdout)
	}
	for _, want := range []string{
		"code_challenge_method=S256",
		"Granted scopes: user:inference user:profile",
		"Lifetime: 365.0 days",
		"Refresh token returned: yes",
		"cproxy accounts add NAME",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr)
		}
	}
	mu.Lock()
	if gotBody["code"] != "goodcode" || gotBody["code_verifier"] != "ver1f13r" ||
		gotBody["grant_type"] != "authorization_code" || gotBody["client_id"] != oauthClientID ||
		gotBody["state"] != "st4t3" || gotBody["expires_in"].(float64) != 31536000 {
		t.Fatalf("exchange body = %v", gotBody)
	}
	mu.Unlock()

	// Wrong state in the pasted code.
	stdout, _, err = run("goodcode#wrong\n")
	if err == nil || !strings.Contains(err.Error(), "not the code for this sign-in") {
		t.Fatalf("bad state err = %v", err)
	}
	if stdout != "" {
		t.Fatalf("bad state stdout = %q", stdout)
	}

	// Upstream rejection.
	mu.Lock()
	failWith = "yes"
	mu.Unlock()
	_, _, err = run("badcode#st4t3\n")
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("HTTP 400 err = %v", err)
	}
}

func TestSetupTokenEOF(t *testing.T) {
	old := oauthTokenEndpoint
	oauthTokenEndpoint = "http://127.0.0.1:0/v1/oauth/token" // must not be reached
	t.Cleanup(func() { oauthTokenEndpoint = old })

	var out, errOut bytes.Buffer
	err := setupTokenExchange(strings.NewReader(""), &out, &errOut, "https://claude.com/cai/oauth/authorize?...", "v", "st4t3", nil, 31536000)
	if err == nil || !strings.Contains(err.Error(), "read code from stdin") {
		t.Fatalf("EOF err = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("EOF stdout = %q", out.String())
	}
}
