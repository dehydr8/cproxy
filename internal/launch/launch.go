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

// Headless Claude Code (claude -p, the Agent SDK) labels its requests
// sdk-cli or sdk-ts, and Anthropic's plan meters currently charge those
// labels roughly 1.7x-2.9x more of the 5-hour window per token than the
// same tokens under any other label (anthropics/claude-code#97074,
// pingdotgg/t3code#24649). A custom label sidesteps the weighting — the
// same workaround T3 Code and Paseo ship — and $CPROXY_ENTRYPOINT opts a
// launch into it. The name must be custom: headless claude rewrites the
// literal "cli" (the interactive TUI's default) to sdk-cli.
const (
	entrypointEnv       = "CLAUDE_CODE_ENTRYPOINT"
	entrypointLabelFlag = "CPROXY_ENTRYPOINT"
)

// BuildChildEnv returns environ rewritten so the child talks to cproxy: the
// base URL and auth token point at the local proxy, competing credentials
// are removed, and every CPROXY_* variable (which may hold secrets) is
// stripped. The prompt cache TTL defaults to 1h, as it would for a signed-in
// subscriber, unless the environment already sets it, and the entrypoint
// label is picked as entrypointFor describes.
func BuildChildEnv(environ []string, baseURL, sessionToken string) []string {
	entrypoint, setEntrypoint := entrypointFor(environ)
	out := make([]string, 0, len(environ)+3)
	hasTTL := false
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if slices.Contains(strippedEnv, k) || strings.HasPrefix(k, "CPROXY_") ||
			(k == entrypointEnv && setEntrypoint) {
			continue
		}
		if k == promptCacheTTLEnv {
			hasTTL = true
		}
		out = append(out, kv)
	}
	if setEntrypoint {
		out = append(out, entrypointEnv+"="+entrypoint)
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

// entrypointFor picks the CLAUDE_CODE_ENTRYPOINT the child runs with. The
// second return is false when the variable should be left exactly as the
// environment has it, including absent — normal behaviour. That is the case
// for $CPROXY_ENTRYPOINT unset, off, 0, or naming a label that cannot work
// (cli, which headless claude rewrites to sdk-cli, and the sdk-* spellings).
// Any other value names the label outright, replacing whatever the
// environment has.
func entrypointFor(environ []string) (string, bool) {
	label, ok := envValue(environ, entrypointLabelFlag)
	if !ok {
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "off", "0":
		return "", false
	}
	if !usableEntrypoint(label) {
		return "", false
	}
	return strings.TrimSpace(label), true
}

// usableEntrypoint reports whether value can serve as the label: the sdk-*
// values are the over-charged category, and headless claude rewrites the
// literal cli to sdk-cli, so those spellings are rejected; any other
// non-empty value works.
func usableEntrypoint(v string) bool {
	v = strings.TrimSpace(v)
	return v != "" && v != "cli" && !strings.HasPrefix(v, "sdk-")
}

// envValue returns the value of name in environ; empty values count as
// unset.
func envValue(environ []string, name string) (string, bool) {
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name && v != "" {
			return v, true
		}
	}
	return "", false
}
