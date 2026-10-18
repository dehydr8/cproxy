package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Client identities of the auto-generated local tokens shared by the claude
// wrapper and the serve instance it starts.
const (
	LocalClientName     = "claude-local" // proxy + usage
	DashboardClientName = "dashboard"    // usage only, for the orb portal
)

// LocalTokenPath is where the auto-generated local client token lives. It
// is not user configuration: the claude wrapper creates it when pool
// accounts exist but no client token is configured, so CPROXY_TOKENS alone
// is enough to route claude through a local serve.
func LocalTokenPath() string { return filepath.Join(Home(), "local-token") }

// DashboardTokenPath holds a usage-only token the claude wrapper creates in
// Amp orbs, so the dashboard portal URL can carry a token that may only
// look, never spend.
func DashboardTokenPath() string { return filepath.Join(Home(), "dashboard-token") }

// EnsureLocalToken returns the local client token, creating it on first use.
func EnsureLocalToken() (string, error) { return ensureTokenFile(LocalTokenPath()) }

// EnsureDashboardToken returns the usage-only dashboard token, creating it on
// first use.
func EnsureDashboardToken() (string, error) { return ensureTokenFile(DashboardTokenPath()) }

// readTokenFile returns the token in path, or "" when there is none.
func readTokenFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// ensureTokenFile returns the token in path, creating it on first use.
// Creation is atomic (write a temp file, then hard-link it into place), so
// concurrent launches all end up with the same token.
func ensureTokenFile(path string) (string, error) {
	if tok, err := readTokenFile(path); err != nil || tok != "" {
		return tok, err
	}
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return "", err
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate local token: %w", err)
	}
	tmp, err := os.CreateTemp(Home(), ".token-*.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString("cp_" + hex.EncodeToString(b) + "\n"); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	// Link fails if another launch won the race; either way, read back
	// whatever is in place.
	if err := os.Link(tmp.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	return readTokenFile(path)
}
