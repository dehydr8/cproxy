// Package launch runs the wrapped command (claude) as a child of cproxy
// with the environment pointed at the local proxy.
package launch

import (
	"slices"
	"strings"
)

// strippedEnv holds variables removed from the wrapped command's environment:
// ambient Anthropic auth would bypass or fight the proxy, and cproxy secrets
// (pool tokens, client tokens) must not leak into the child.
var strippedEnv = []string{
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_API_KEY",
	"CLAUDE_CODE_OAUTH_TOKEN",
}

// BuildChildEnv returns environ rewritten so the child talks to cproxy: the
// base URL and auth token point at the local proxy, competing credentials
// are removed, and every CPROXY_* variable (which may hold secrets) is
// stripped.
func BuildChildEnv(environ []string, baseURL, sessionToken string) []string {
	out := make([]string, 0, len(environ)+2)
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if slices.Contains(strippedEnv, k) || strings.HasPrefix(k, "CPROXY_") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		"ANTHROPIC_BASE_URL="+baseURL,
		"ANTHROPIC_AUTH_TOKEN="+sessionToken,
	)
	return out
}
