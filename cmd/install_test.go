package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dehydr8/cproxy/internal/wrap"
)

// Both commands must accept --path end to end: it was once registered only on
// install, so `cproxy uninstall --path ...` failed to parse.
func TestInstallUninstallCommandsAcceptPath(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude")
	orig := "#!/bin/sh\necho real claude\n"
	if err := os.WriteFile(claude, []byte(orig), 0o755); err != nil {
		t.Fatal(err)
	}

	rootCmd.SetArgs([]string{"install", "--path", claude})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("install --path: %v", err)
	}
	if !wrap.IsShim(claude) {
		t.Fatal("claude was not replaced by the shim after install --path")
	}

	rootCmd.SetArgs([]string{"uninstall", "--path", claude})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("uninstall --path: %v", err)
	}
	got, err := os.ReadFile(claude)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != orig {
		t.Fatalf("claude not restored after uninstall: %q", got)
	}
}
