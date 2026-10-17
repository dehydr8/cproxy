//go:build unix

package launch

import (
	"os"
	"slices"
	"testing"
)

func TestBuildChildEnv(t *testing.T) {
	env := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/u",
		"ANTHROPIC_BASE_URL=https://api.anthropic.com",
		"ANTHROPIC_AUTH_TOKEN=ambient",
		"ANTHROPIC_API_KEY=sk-ambient",
		"CLAUDE_CODE_OAUTH_TOKEN=ambient",
		"ANTHROPIC_MODEL=opus",
		"CPROXY_TOKENS=a:secret-a",
		"CPROXY_POOL_TOKENS_CVP=b:secret-b",
		"CPROXY_CLIENT_TOKENS=alice:secret",
		"CPROXY_HOME=/tmp/elsewhere",
	}
	got := BuildChildEnv(env, "http://127.0.0.1:4123", "cp_sess")

	set := map[string]string{}
	for _, kv := range got {
		k, v, _ := cut(kv)
		set[k] = v
	}
	if set["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:4123" {
		t.Fatalf("base URL = %q", set["ANTHROPIC_BASE_URL"])
	}
	if set["ANTHROPIC_AUTH_TOKEN"] != "cp_sess" {
		t.Fatalf("auth token = %q", set["ANTHROPIC_AUTH_TOKEN"])
	}
	for _, banned := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "CPROXY_TOKENS", "CPROXY_CLIENT_TOKENS", "CPROXY_HOME"} {
		if _, ok := set[banned]; ok {
			t.Fatalf("child env still contains %s", banned)
		}
	}
	for _, kv := range got {
		k, _, ok := cut(kv)
		if !ok {
			t.Fatalf("malformed env entry %q", kv)
		}
		if len(k) >= 7 && k[:7] == "CPROXY_" {
			t.Fatalf("child env still contains CPROXY_* entry %q", kv)
		}
	}
	if !slices.Contains(got, "PATH=/usr/bin:/bin") || !slices.Contains(got, "HOME=/home/u") || !slices.Contains(got, "ANTHROPIC_MODEL=opus") {
		t.Fatalf("unrelated vars dropped: %v", got)
	}
}

func cut(kv string) (string, string, bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return kv, "", false
}

func TestRunExitCode(t *testing.T) {
	code, err := Run([]string{"sh", "-c", "exit 7"}, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 {
		t.Fatalf("code = %d, want 7", code)
	}
}

func TestRunSignalExitCode(t *testing.T) {
	code, err := Run([]string{"sh", "-c", "kill -TERM $$"}, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	if code != 143 {
		t.Fatalf("code = %d, want 143 (128+SIGTERM)", code)
	}
}

func TestRunMissingCommand(t *testing.T) {
	if _, err := Run(nil, os.Environ()); err == nil {
		t.Fatal("nil argv accepted")
	}
}
