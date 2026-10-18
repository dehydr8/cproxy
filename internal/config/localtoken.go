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

// LocalClientName is the client identity of the auto-generated local token
// shared by the claude wrapper and the serve instance it starts.
const LocalClientName = "claude-local"

// LocalTokenPath is where the auto-generated local client token lives. It
// is not user configuration: the claude wrapper creates it when pool
// accounts exist but no client token is configured, so CPROXY_TOKENS alone
// is enough to route claude through a local serve.
func LocalTokenPath() string { return filepath.Join(Home(), "local-token") }

// readLocalToken returns the local token, or "" when there is none.
func readLocalToken() (string, error) {
	b, err := os.ReadFile(LocalTokenPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// EnsureLocalToken returns the local client token, creating it on first
// use. Creation is atomic (write a temp file, then hard-link it into place),
// so concurrent launches all end up with the same token.
func EnsureLocalToken() (string, error) {
	if tok, err := readLocalToken(); err != nil || tok != "" {
		return tok, err
	}
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return "", err
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate local token: %w", err)
	}
	tmp, err := os.CreateTemp(Home(), ".local-token-*.tmp")
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
	if err := os.Link(tmp.Name(), LocalTokenPath()); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	return readLocalToken()
}
