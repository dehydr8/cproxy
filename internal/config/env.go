package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/dehydr8/cproxy/internal/pool"
)

// envToken is one parsed name/token pair from a token-list variable.
type envToken struct {
	Name  string
	Token string
}

// parseTokenList parses "name:tok,name2:tok2,baretok", separated by commas
// and/or newlines. Entries without a "name:" prefix get generated names.
// When a name repeats, the later entry wins.
func parseTokenList(list, genPrefix string) []envToken {
	var raw []envToken
	gen := 0
	for _, part := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == '\n' }) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, tok := "", part
		if i := strings.Index(part, ":"); i > 0 {
			name, tok = strings.TrimSpace(part[:i]), part[i+1:]
		}
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if name == "" {
			gen++
			name = fmt.Sprintf("%s-%d", genPrefix, gen)
		}
		raw = append(raw, envToken{name, tok})
	}
	// Deduplicate by name, keeping the last occurrence.
	out := raw[:0]
	for _, e := range raw {
		replaced := false
		for i := range out {
			if out[i].Name == e.Name {
				out[i] = e
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, e)
		}
	}
	return out
}

// envAccounts collects pool members from the environment:
//
//	CPROXY_TOKENS=name:tok,…            list → default pool
//	CPROXY_POOL_TOKENS_<POOL>=name:tok,… list → POOL
//	CPROXY_TOKEN_<NAME>=tok              one account → default pool
//	CPROXY_POOL_TOKEN_<POOL>_<NAME>=tok  one account "<pool>-<name>" → POOL
//
// Names are lowercased. In the per-account form the pool is the first
// segment after the prefix, so pool names cannot contain "_"; the account
// name is prefixed with the pool because account names are global (health
// state is keyed by them). A name that repeats keeps its last definition,
// and a token that repeats under several names becomes one account in all
// of their pools, named by the plainest form, so one subscription is never
// tracked as two.
func envAccounts(environ []string) ([]pool.Account, error) {
	vars := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && v != "" {
			vars[k] = v
		}
	}
	// Read by form, then by name: when one token appears under several
	// names, the plainest name (CPROXY_TOKENS, then CPROXY_TOKEN_*) wins.
	form := func(k string) int {
		switch {
		case k == EnvTokens:
			return 0
		case strings.HasPrefix(k, EnvTokenPrefix):
			return 1
		case strings.HasPrefix(k, EnvPoolPrefix):
			return 2
		default:
			return 3
		}
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if fa, fb := form(a), form(b); fa != fb {
			return fa - fb
		}
		return strings.Compare(a, b)
	})

	var out []pool.Account
	add := func(name, tok, poolName string) {
		a := pool.Account{
			Name:   name,
			Token:  tok,
			Type:   pool.DetectTokenType(tok),
			Pools:  []string{poolName},
			Source: "env",
		}
		if i := slices.IndexFunc(out, func(x pool.Account) bool { return x.Name == name }); i >= 0 {
			out[i] = a
			return
		}
		out = append(out, a)
	}
	for _, k := range keys {
		v := vars[k]
		switch {
		case k == EnvTokens:
			for _, e := range parseTokenList(v, "env") {
				add(e.Name, e.Token, DefaultPoolName)
			}
		case strings.HasPrefix(k, EnvPoolPrefix) && len(k) > len(EnvPoolPrefix):
			p := strings.ToLower(k[len(EnvPoolPrefix):])
			for _, e := range parseTokenList(v, "env") {
				add(e.Name, e.Token, p)
			}
		case strings.HasPrefix(k, EnvPoolTokenPref):
			p, n, ok := strings.Cut(k[len(EnvPoolTokenPref):], "_")
			if !ok || p == "" || n == "" {
				return nil, fmt.Errorf("%s: want %s<POOL>_<NAME> (pool names cannot contain \"_\")", k, EnvPoolTokenPref)
			}
			p = strings.ToLower(p)
			add(p+"-"+strings.ToLower(n), strings.TrimSpace(v), p)
		case strings.HasPrefix(k, EnvTokenPrefix):
			n := k[len(EnvTokenPrefix):]
			if n == "" {
				return nil, fmt.Errorf("%s: want %s<NAME>", k, EnvTokenPrefix)
			}
			add(strings.ToLower(n), strings.TrimSpace(v), DefaultPoolName)
		}
	}

	// One token under several names: keep the first account, in every pool.
	merged := out[:0]
	for _, a := range out {
		if i := slices.IndexFunc(merged, func(x pool.Account) bool { return x.Token == a.Token }); i >= 0 {
			for _, p := range a.Pools {
				if !slices.Contains(merged[i].Pools, p) {
					merged[i].Pools = append(merged[i].Pools, p)
				}
			}
			continue
		}
		merged = append(merged, a)
	}
	return merged, nil
}

// envClientTokens parses CPROXY_CLIENT_TOKENS into named specs.
func envClientTokens(environ []string) map[string]ClientTokenSpec {
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok && k == EnvClientTokens && v != "" {
			return ClientTokensFromList(v)
		}
	}
	return nil
}

// NamedClientToken is a client token together with the name it was
// configured under. ClientTokensFromList returns a map and loses the list
// order; the claude wrapper needs it to pick "the first token".
type NamedClientToken struct {
	Name string
	Spec ClientTokenSpec
}

// ClientTokenList parses a "name:token[:scopes],…" list IN ORDER.
func ClientTokenList(list string) []NamedClientToken {
	var out []NamedClientToken
	for _, e := range parseTokenList(list, "client") {
		spec := ClientTokenSpec{Token: e.Token}
		if tok, scopes, ok := strings.Cut(e.Token, ":"); ok {
			spec.Token = strings.TrimSpace(tok)
			for _, s := range strings.Split(scopes, "+") {
				if s = strings.TrimSpace(s); s != "" {
					spec.Scopes = append(spec.Scopes, s)
				}
			}
		}
		if spec.Token == "" {
			continue
		}
		out = append(out, NamedClientToken{Name: e.Name, Spec: spec})
	}
	return out
}

// EnvClientTokenList returns the CPROXY_CLIENT_TOKENS entries in list order.
func EnvClientTokenList(environ []string) []NamedClientToken {
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == EnvClientTokens && v != "" {
			return ClientTokenList(v)
		}
	}
	return nil
}

// EnvAuthToken returns CPROXY_AUTH_TOKEN from environ, or "".
func EnvAuthToken(environ []string) string {
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == EnvAuthTokenName && v != "" {
			return v
		}
	}
	return ""
}

// EnvPort returns CPROXY_PORT from environ, or "".
func EnvPort(environ []string) string {
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == EnvPortName && v != "" {
			return v
		}
	}
	return ""
}

// EnvUpstream returns CPROXY_UPSTREAM from environ, or "".
func EnvUpstream(environ []string) string {
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == EnvUpstreamName && v != "" {
			return v
		}
	}
	return ""
}

// ClientTokensFromList parses a "name:token[:scopes],…" list (as used by
// --client-tokens and CPROXY_CLIENT_TOKENS) into named specs. Entries
// without a name get generated names. The optional third field lists the
// token's scopes, "+"-separated (e.g. "monitor:cp_…:usage" or
// "alice:cp_…:proxy+usage"); without it the token gets full access.
func ClientTokensFromList(list string) map[string]ClientTokenSpec {
	specs := map[string]ClientTokenSpec{}
	for _, e := range ClientTokenList(list) {
		specs[e.Name] = e.Spec
	}
	return specs
}

// EnvPool returns CPROXY_POOL from environ, or "".
func EnvPool(environ []string) string {
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == EnvPoolName && v != "" {
			return v
		}
	}
	return ""
}
