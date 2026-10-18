// Package config loads, validates and resolves cproxy configuration from
// ~/.cproxy/credentials.json and the environment.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dehydr8/cproxy/internal/pool"
)

// Environment variables understood by cproxy.
const (
	EnvHome          = "CPROXY_HOME"          // overrides ~/.cproxy
	EnvTokens        = "CPROXY_TOKENS"        // name:token,… → default pool
	EnvPoolPrefix    = "CPROXY_POOL_TOKENS_"  // + POOLNAME → that pool
	EnvClientTokens  = "CPROXY_CLIENT_TOKENS" // name:token,… serve-mode client auth
	EnvUpstreamName  = "CPROXY_UPSTREAM"      // upstream base URL override
	EnvAuthTokenName = "CPROXY_AUTH_TOKEN"    // client token the claude wrapper presents
	EnvPortName      = "CPROXY_PORT"          // port of the wrapper's shared serve instance
	EnvPoolName      = "CPROXY_POOL"          // pool used when --pool is not given
	EnvTokenPrefix   = "CPROXY_TOKEN_"        // + NAME → one default-pool account
	EnvPoolTokenPref = "CPROXY_POOL_TOKEN_"   // + POOL_NAME → one account in POOL
)

// DefaultUpstream is the Anthropic API base URL used unless overridden.
const DefaultUpstream = "https://api.anthropic.com"

// DefaultPoolName is the pool used when neither --pool nor CPROXY_POOL is
// given. Accounts that list no pools belong to it.
const DefaultPoolName = "default"

// FileConfig is the on-disk shape of ~/.cproxy/credentials.json.
type FileConfig struct {
	Version      int                        `json:"version,omitempty"`
	Accounts     []pool.Account             `json:"accounts,omitempty"`
	Pools        map[string]PoolConfig      `json:"pools,omitempty"`
	ClientTokens map[string]ClientTokenSpec `json:"client_tokens,omitempty"`

	// LocalToken is the auto-generated local client token (LocalTokenPath),
	// filled in by Load and never written back to credentials.json.
	LocalToken string `json:"-"`
}

// PoolConfig tweaks a named pool.
type PoolConfig struct {
	Strategy string `json:"strategy,omitempty"` // see pool.Strategies()
}

// ClientTokenSpec is a serve-mode client credential. JSON accepts either a
// plain token or an object with a token, an optional per-client pool and
// optional scopes limiting what the token may do:
//
//	"alice": "cp_…"
//	"alice": {"token": "cp_…", "pool": "cvp"}
//	"monitor": {"token": "cp_…", "scopes": ["usage"]}
type ClientTokenSpec struct {
	Token  string
	Pool   string
	Scopes []string
}

// Scopes a client token can carry. By default (no scopes listed) a token may
// do both; a usage-only token can open the dashboard but not use the proxy.
const (
	ScopeProxy = "proxy" // send requests through the proxy
	ScopeUsage = "usage" // view the /usage dashboard and JSON
)

// AllScopes lists the accepted scope names for help text.
var AllScopes = []string{ScopeProxy, ScopeUsage}

// ParseScopes validates and normalizes a scope list. An empty list means
// "all scopes" (full access) for backward compatibility.
func ParseScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return slices.Clone(AllScopes), nil
	}
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if !slices.Contains(AllScopes, s) {
			return nil, fmt.Errorf("unknown scope %q (want one or more of: %s)", s, strings.Join(AllScopes, ", "))
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty scope list")
	}
	return out, nil
}

// UnmarshalJSON accepts the string or object forms.
func (c *ClientTokenSpec) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return errors.New("empty client token")
		}
		c.Token = s
		return nil
	}
	var raw struct {
		Token  string   `json:"token"`
		Pool   string   `json:"pool"`
		Scopes []string `json:"scopes"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if strings.TrimSpace(raw.Token) == "" {
		return errors.New("empty client token")
	}
	c.Token, c.Pool, c.Scopes = raw.Token, raw.Pool, raw.Scopes
	return nil
}

// MarshalJSON writes back the same forms.
func (c ClientTokenSpec) MarshalJSON() ([]byte, error) {
	if c.Pool == "" && len(c.Scopes) == 0 {
		return json.Marshal(c.Token)
	}
	obj := struct {
		Token  string   `json:"token"`
		Pool   string   `json:"pool,omitempty"`
		Scopes []string `json:"scopes,omitempty"`
	}{Token: c.Token, Pool: c.Pool, Scopes: c.Scopes}
	return json.Marshal(obj)
}

// Home returns the cproxy config directory ($CPROXY_HOME or ~/.cproxy).
func Home() string {
	if h := os.Getenv(EnvHome); h != "" {
		return h
	}
	base, err := os.UserHomeDir()
	if err != nil {
		base = "."
	}
	return filepath.Join(base, ".cproxy")
}

// Path returns the credentials file location.
func Path() string { return filepath.Join(Home(), "credentials.json") }

// Load reads credentials.json (a missing file yields an empty config) and
// the auto-generated local client token, if any.
func Load() (*FileConfig, error) {
	local, err := readLocalToken()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(Path())
	if errors.Is(err, os.ErrNotExist) {
		return &FileConfig{LocalToken: local}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg FileConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", Path(), err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", Path(), err)
	}
	cfg.LocalToken = local
	return &cfg, nil
}

// Save atomically writes credentials.json with private permissions.
func Save(cfg *FileConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return err
	}
	if cfg.Version == 0 {
		cfg.Version = 1
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(Home(), ".credentials-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), Path())
}

// Validate checks the file config for contradictions.
func (f *FileConfig) Validate() error {
	names := map[string]bool{}
	for i, a := range f.Accounts {
		if strings.TrimSpace(a.Name) == "" {
			return fmt.Errorf("accounts[%d]: empty name", i)
		}
		if names[a.Name] {
			return fmt.Errorf("duplicate account name %q", a.Name)
		}
		names[a.Name] = true
		if strings.TrimSpace(a.Token) == "" {
			return fmt.Errorf("account %q: empty token", a.Name)
		}
		for _, p := range a.Pools {
			if strings.TrimSpace(p) == "" {
				return fmt.Errorf("account %q: empty pool name", a.Name)
			}
		}
	}
	for name, pc := range f.Pools {
		if _, err := pool.ParseStrategy(pc.Strategy); err != nil {
			return fmt.Errorf("pool %q: %w", name, err)
		}
	}
	seen := map[string]string{}
	for name, ct := range f.ClientTokens {
		if strings.TrimSpace(ct.Token) == "" {
			return fmt.Errorf("client token %q: empty token", name)
		}
		if other, dup := seen[ct.Token]; dup {
			return fmt.Errorf("client tokens %q and %q share the same secret", other, name)
		}
		seen[ct.Token] = name
		if _, err := ParseScopes(ct.Scopes); err != nil {
			return fmt.Errorf("client token %q: %w", name, err)
		}
	}
	return nil
}

// ClientAuth is a resolved serve-mode client identity.
type ClientAuth struct {
	Name   string
	Pool   string   // optional per-client pool override
	Scopes []string // what this token may do; empty means full access
}

// HasScope reports whether the client may do something. Clients with no
// scopes recorded (run mode's local session, --no-auth) have full access.
func (ca ClientAuth) HasScope(scope string) bool {
	if len(ca.Scopes) == 0 {
		return true
	}
	return slices.Contains(ca.Scopes, scope)
}

// Runtime merges file and environment configuration into what the proxy
// needs at request time.
type Runtime struct {
	Accounts    []pool.Account
	Pools       map[string]PoolConfig
	ClientAuth  map[string]ClientAuth // client token -> identity
	DefaultPool string                // pool of accounts that list none
	Pool        string                // pool for clients without their own (--pool, CPROXY_POOL); DefaultPool when empty
}

// SelectedPool is the pool requests use unless their client names another.
func (rt *Runtime) SelectedPool() string {
	if rt.Pool != "" {
		return rt.Pool
	}
	return rt.DefaultPool
}

// PoolFor is the pool a client's requests draw from.
func (rt *Runtime) PoolFor(ca ClientAuth) string {
	if ca.Pool != "" {
		return ca.Pool
	}
	return rt.SelectedPool()
}

// Build resolves the file config plus the process environment. Environment
// accounts override same-named file accounts; environment client tokens
// override same-named file client tokens.
func Build(file *FileConfig, environ []string) (*Runtime, error) {
	rt := &Runtime{
		Accounts:    append([]pool.Account(nil), file.Accounts...),
		Pools:       map[string]PoolConfig{},
		ClientAuth:  map[string]ClientAuth{},
		DefaultPool: DefaultPoolName,
		Pool:        strings.ToLower(strings.TrimSpace(EnvPool(environ))),
	}
	for n, p := range file.Pools {
		rt.Pools[n] = p
	}
	for n, spec := range file.ClientTokens {
		scopes, err := ParseScopes(spec.Scopes)
		if err != nil {
			return nil, fmt.Errorf("client token %q: %w", n, err)
		}
		rt.ClientAuth[spec.Token] = ClientAuth{Name: n, Pool: spec.Pool, Scopes: scopes}
	}

	envAccts, err := envAccounts(environ)
	if err != nil {
		return nil, err
	}
	for _, a := range envAccts {
		if i := slices.IndexFunc(rt.Accounts, func(x pool.Account) bool { return x.Name == a.Name }); i >= 0 {
			rt.Accounts[i] = a
		} else {
			rt.Accounts = append(rt.Accounts, a)
		}
	}
	for n, spec := range envClientTokens(environ) {
		scopes, err := ParseScopes(spec.Scopes)
		if err != nil {
			return nil, fmt.Errorf("client token %q: %w", n, err)
		}
		for tok, ca := range rt.ClientAuth {
			if ca.Name == n {
				delete(rt.ClientAuth, tok)
			}
		}
		rt.ClientAuth[spec.Token] = ClientAuth{Name: n, Pool: spec.Pool, Scopes: scopes}
	}

	// The auto-generated local token authenticates the claude wrapper with
	// full access, unless the same secret is already configured explicitly.
	if file.LocalToken != "" {
		if _, ok := rt.ClientAuth[file.LocalToken]; !ok {
			rt.ClientAuth[file.LocalToken] = ClientAuth{Name: LocalClientName, Scopes: slices.Clone(AllScopes)}
		}
	}

	// Hand-edited file accounts may omit "type"; the proxy must still send
	// an API key as x-api-key, and only OAuth accounts get usage polling.
	for i := range rt.Accounts {
		if rt.Accounts[i].Type == "" {
			rt.Accounts[i].Type = pool.DetectTokenType(rt.Accounts[i].Token)
		}
	}

	if err := rt.validate(); err != nil {
		return nil, err
	}
	return rt, nil
}

func (rt *Runtime) validate() error {
	names := map[string]bool{}
	for _, a := range rt.Accounts {
		if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Token) == "" {
			return fmt.Errorf("invalid account %q (needs a name and a token)", a.Name)
		}
		if names[a.Name] {
			return fmt.Errorf("duplicate account name %q", a.Name)
		}
		names[a.Name] = true
	}
	for n, pc := range rt.Pools {
		if _, err := pool.ParseStrategy(pc.Strategy); err != nil {
			return fmt.Errorf("pool %q: %w", n, err)
		}
	}
	return nil
}

// AccountPools returns the pools an account belongs to; accounts that list
// none belong to the default pool.
func (rt *Runtime) AccountPools(a pool.Account) []string {
	if len(a.Pools) == 0 {
		return []string{rt.DefaultPool}
	}
	return a.Pools
}

// PoolNames returns every pool name referenced by any account or pool config,
// sorted.
func (rt *Runtime) PoolNames() []string {
	seen := map[string]bool{}
	for _, a := range rt.Accounts {
		for _, p := range rt.AccountPools(a) {
			seen[p] = true
		}
	}
	for n := range rt.Pools {
		seen[n] = true
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// ResolvePool returns the usable members of the named pool (SelectedPool
// when name is empty) together with its strategy. Disabled accounts are
// excluded. An unknown pool name yields an error listing the known pools.
func (rt *Runtime) ResolvePool(name string) ([]pool.Account, pool.Strategy, error) {
	if name == "" {
		name = rt.SelectedPool()
	}
	strategy := pool.DefaultStrategy
	if pc, ok := rt.Pools[name]; ok {
		s, err := pool.ParseStrategy(pc.Strategy)
		if err != nil {
			return nil, "", fmt.Errorf("pool %q: %w", name, err)
		}
		strategy = s
	}

	var members []pool.Account
	anyRef := false
	for _, a := range rt.Accounts {
		if !slices.Contains(rt.AccountPools(a), name) {
			continue
		}
		anyRef = true
		if !a.Disabled {
			members = append(members, a)
		}
	}
	if _, ok := rt.Pools[name]; ok {
		anyRef = true
	}
	if len(members) == 0 {
		if !anyRef {
			return nil, "", fmt.Errorf("unknown pool %q (available: %s)", name, strings.Join(rt.PoolNames(), ", "))
		}
		return nil, "", fmt.Errorf("pool %q has no usable members (all disabled)", name)
	}
	return members, strategy, nil
}
