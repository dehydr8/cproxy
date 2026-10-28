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

// promptCacheTTLEnv sets the main conversation's prompt cache TTL. Claude
// Code picks 1 hour only when it is signed in to claude.ai, and with
// ANTHROPIC_AUTH_TOKEN set it assumes it is not, falling back to 5 minutes
// although every pool account is a subscription.
const promptCacheTTLEnv = "CLAUDE_CODE_PROMPT_CACHE_TTL"

// BuildChildEnv returns environ rewritten so the child talks to cproxy: the
// base URL and auth token point at the local proxy, competing credentials
// are removed, and every CPROXY_* variable (which may hold secrets) is
// stripped. The prompt cache TTL defaults to 1h, as it would for a signed-in
// subscriber, unless the environment already sets it.
func BuildChildEnv(environ []string, baseURL, sessionToken string) []string {
	out := make([]string, 0, len(environ)+3)
	hasTTL := false
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if slices.Contains(strippedEnv, k) || strings.HasPrefix(k, "CPROXY_") {
			continue
		}
		if k == promptCacheTTLEnv {
			hasTTL = true
		}
		out = append(out, kv)
	}
	if !hasTTL {
		out = append(out, promptCacheTTLEnv+"=1h")
	}
	out = append(out,
		"ANTHROPIC_BASE_URL="+baseURL,
		"ANTHROPIC_AUTH_TOKEN="+sessionToken,
	)
	return out
}
