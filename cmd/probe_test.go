package cmd

import (
	"slices"
	"testing"
)

func TestProbeEnvIsolatesClaude(t *testing.T) {
	env := probeEnv([]string{
		"PATH=/bin", "HOME=/h",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:7788", // would send the probe to a proxy
		"ANTHROPIC_AUTH_TOKEN=cp_x",
		"CLAUDE_CODE_OAUTH_TOKEN=someone-else",
		"CLAUDECODE=1",
		"CPROXY_TOKEN_A=sk-ant-oat01-a", // would make a shim route the probe
	}, "sk-ant-oat01-probe", "/tmp/cfg")
	want := []string{"PATH=/bin", "HOME=/h",
		"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-probe", "MAX_THINKING_TOKENS=0", "CLAUDE_CONFIG_DIR=/tmp/cfg"}
	if !slices.Equal(env, want) {
		t.Fatalf("probe env = %q\nwant        %q", env, want)
	}
}
