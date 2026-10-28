package cmd

import (
	"os"
	"strings"
	"testing"
)

// TestMain runs the tests with no ambient cproxy or Anthropic configuration:
// the developer's own pool tokens must never leak into a test (or its
// failure output), and every test sets exactly what it needs.
func TestMain(m *testing.M) {
	// Tests that start the daemon spawn this binary as `<test> daemon`.
	if len(os.Args) == 2 && os.Args[1] == "daemon" {
		rootCmd.SetArgs([]string{"daemon"})
		Execute()
		os.Exit(0)
	}
	scrubEnv()
	os.Exit(m.Run())
}

func scrubEnv() {
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "CPROXY_") || strings.HasPrefix(k, "ANTHROPIC_") ||
			k == "CLAUDE_CODE_OAUTH_TOKEN" {
			os.Unsetenv(k)
		}
	}
}
