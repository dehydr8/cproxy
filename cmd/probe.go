package cmd

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dehydr8/cproxy/internal/proxy"
	"github.com/dehydr8/cproxy/internal/wrap"
)

// One real Claude Code turn: the cheapest model, no tools, no settings, MCP
// or history, a one-line system prompt: a few hundred input tokens and one
// output token (claude-swap's probe).
var probeArgs = []string{
	"-p", ".", "--output-format", "stream-json", "--verbose",
	"--model", "haiku", "--max-turns", "1", "--tools", "",
	"--system-prompt", "Reply with one word.", "--setting-sources", "",
	"--strict-mcp-config", "--no-session-persistence",
}

// claudeUsageProbe returns the Haiku probe for OAuth tokens the usage API
// refuses: it runs one minimal turn of the real claude binary with the
// token and reads the windows Claude Code reports in its rate_limit_event.
// Real Claude Code traffic rather than a hand-built request, as claude-swap
// does. Nil when no claude binary is installed (e.g. a remote serve).
func claudeUsageProbe() proxy.UsageProbe {
	bin, err := discoverClaude("")
	if err != nil {
		return nil
	}
	if wrap.IsShim(bin) {
		bin = wrap.BackupPath(bin) // never route the probe back through cproxy
	}
	if _, err := os.Stat(bin); err != nil {
		return nil
	}
	return func(ctx context.Context, token string) (*proxy.UsageAPI, error) {
		tmp, err := os.MkdirTemp("", "cproxy-probe-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmp)
		cmd := exec.CommandContext(ctx, bin, probeArgs...)
		cmd.Dir = tmp
		cmd.Env = probeEnv(os.Environ(), token, tmp)
		out, _ := cmd.Output() // a failed turn still reports why in its transcript
		return proxy.ParseRateLimitEvents(string(out), time.Now())
	}
}

// probeEnv drops every Anthropic, Claude Code and cproxy setting (a base
// URL would send the probe to a proxy; CPROXY_* would make a shim route it)
// and points claude at the token and a throwaway config directory.
func probeEnv(environ []string, token, configDir string) []string {
	var out []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "ANTHROPIC_") || strings.HasPrefix(k, "CLAUDE_") ||
			strings.HasPrefix(k, "CPROXY_") || k == "CLAUDECODE" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "CLAUDE_CODE_OAUTH_TOKEN="+token, "MAX_THINKING_TOKENS=0", "CLAUDE_CONFIG_DIR="+configDir)
}
