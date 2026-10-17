package config

import (
	"fmt"
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

// envAccounts collects pool members from CPROXY_TOKENS and
// CPROXY_POOL_TOKENS_<NAME> variables.
func envAccounts(environ []string) []pool.Account {
	var out []pool.Account
	add := func(list, poolName string) {
		for _, e := range parseTokenList(list, "env") {
			out = append(out, pool.Account{
				Name:   e.Name,
				Token:  e.Token,
				Type:   pool.DetectTokenType(e.Token),
				Pools:  []string{poolName},
				Source: "env",
			})
		}
	}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" {
			continue
		}
		switch {
		case k == EnvTokens:
			add(v, DefaultPoolName)
		case len(k) > len(EnvPoolPrefix) && strings.HasPrefix(k, EnvPoolPrefix):
			add(v, strings.ToLower(k[len(EnvPoolPrefix):]))
		}
	}
	// A name repeated across variables: the later variable wins.
	merged := out[:0]
	for _, a := range out {
		replaced := false
		for i := range merged {
			if merged[i].Name == a.Name {
				merged[i] = a
				replaced = true
				break
			}
		}
		if !replaced {
			merged = append(merged, a)
		}
	}
	return merged
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

// ClientTokensFromList parses a "name:token[:scopes],…" list (as used by
// --client-tokens and CPROXY_CLIENT_TOKENS) into named specs. Entries
// without a name get generated names. The optional third field lists the
// token's scopes, "+"-separated (e.g. "monitor:cp_…:usage" or
// "alice:cp_…:proxy+usage"); without it the token gets full access.
func ClientTokensFromList(list string) map[string]ClientTokenSpec {
	specs := map[string]ClientTokenSpec{}
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
		specs[e.Name] = spec
	}
	return specs
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
