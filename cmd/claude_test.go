package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dehydr8/cproxy/internal/config"
	"github.com/dehydr8/cproxy/internal/wrap"
)

func TestResolveRealClaudeUnwrapsShim(t *testing.T) {
	dir := t.TempDir()
	backup := wrap.BackupPath(filepath.Join(dir, "claude"))
	if err := os.WriteFile(backup, []byte("#!/bin/sh\nreal claude\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	shim := wrap.ShimScript(filepath.Join(dir, "cproxy"), backup)
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	got, err := resolveRealClaude("")
	if err != nil {
		t.Fatal(err)
	}
	if got != backup {
		t.Fatalf("resolved %q, want the shim's backup %q", got, backup)
	}
}

func TestResolveRealClaudeMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := resolveRealClaude(""); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want a not-found error", err)
	}
}

// fakeClaude writes the environment and args it was launched with to out.
func fakeClaude(t *testing.T, out string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"echo \"base=$ANTHROPIC_BASE_URL auth=$ANTHROPIC_AUTH_TOKEN args=$*\" > \"" + out + "\"\n"
	fake := filepath.Join(dir, "claude")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return fake
}

func TestRunClaudePassThroughForeignRouting(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	out := filepath.Join(t.TempDir(), "out")
	fakeClaude(t, out)

	t.Setenv("ANTHROPIC_BASE_URL", "https://somewhere-else")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-ant-not-cproxy")

	if err := runClaude(claudeCmd, []string{"hello"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "base=https://somewhere-else") ||
		!strings.Contains(string(got), "auth=sk-ant-not-cproxy") ||
		!strings.Contains(string(got), "args=hello") {
		t.Fatalf("claude ran with %q; its own routing should be untouched", got)
	}
}

func TestRunClaudePassThroughUnconfigured(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	out := filepath.Join(t.TempDir(), "out")
	fakeClaude(t, out)

	if err := runClaude(claudeCmd, []string{"-p", "hi"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "base= auth=") || !strings.Contains(string(got), "args=-p hi") {
		t.Fatalf("claude ran with %q; unconfigured cproxy should stay out of the way", got)
	}
}
