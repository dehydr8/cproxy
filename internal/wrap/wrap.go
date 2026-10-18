// Package wrap turns the installed claude binary into a shim over cproxy:
// Install replaces it with a tiny script that runs `cproxy claude`, and
// Decide picks the client credential that command presents to the shared
// serve instance.
package wrap

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dehydr8/cproxy/internal/config"
)

// Marker identifies a shim installed by cproxy.
const Marker = "# cproxy claude shim"

// BackupSuffix is appended to the claude path to name the real binary
// Install moved aside (claude → claude.cproxy-real).
const BackupSuffix = ".cproxy-real"

// Errors reported by Install and Uninstall; callers turn them into
// friendly messages.
var (
	ErrAlreadyInstalled = errors.New("cproxy shim is already installed at this path")
	ErrNotInstalled     = errors.New("no cproxy shim is installed at this path")
	// ErrNoClientToken means pool accounts are configured but no client
	// token is; `cproxy claude` answers it by creating the local token
	// (config.EnsureLocalToken) and deciding again.
	ErrNoClientToken = errors.New("pool accounts are configured but no client token is")
)

// ShimScript is the script written over the claude binary. It is
// deliberately tiny and holds no secrets and no decisions: every choice
// (credentials present? which port? back off?) happens again in
// `cproxy claude` on each launch, so config changes never need a reinstall.
func ShimScript(cproxyPath, realClaude string) string {
	return "#!/bin/sh\n" +
		Marker + " (real binary: " + strings.ReplaceAll(realClaude, "\n", " ") + "; installed by `cproxy install`)\n" +
		"exec " + shQuote(cproxyPath) + " claude --exec " + shQuote(realClaude) + " -- \"$@\"\n"
}

// shQuote single-quotes s for sh, so paths with spaces, quotes, $ or
// backticks reach exec verbatim.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// IsShim reports whether path holds a shim written by Install.
func IsShim(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head, err := io.ReadAll(io.LimitReader(f, 512))
	if err != nil {
		return false
	}
	return bytes.Contains(head, []byte(Marker))
}

// BackupPath returns where Install moves the real binary for a claude path.
func BackupPath(claudePath string) string {
	return claudePath + BackupSuffix
}

// Install replaces claudePath with the shim, moving the real binary to
// BackupPath(claudePath). The move is rolled back if the shim cannot be
// written. Re-running it after Claude Code updates itself (its updater may
// overwrite the shim) refreshes the shim and the backup.
func Install(claudePath string) (shim, backup string, err error) {
	if IsShim(claudePath) {
		return "", "", ErrAlreadyInstalled
	}
	info, err := os.Stat(claudePath)
	if err != nil {
		return "", "", err
	}
	if info.IsDir() {
		return "", "", fmt.Errorf("%s is a directory", claudePath)
	}
	cproxy, err := os.Executable()
	if err != nil {
		return "", "", err
	}

	backup = BackupPath(claudePath)
	if err := os.Rename(claudePath, backup); err != nil {
		return "", "", fmt.Errorf("move the real claude binary aside: %w", err)
	}
	if err := os.WriteFile(claudePath, []byte(ShimScript(cproxy, backup)), info.Mode().Perm()); err != nil {
		os.Rename(backup, claudePath) // best-effort rollback
		return "", "", fmt.Errorf("write the shim: %w", err)
	}
	return claudePath, backup, nil
}

// Uninstall restores the real binary moved aside by Install. It only
// replaces a path that is actually a cproxy shim.
func Uninstall(claudePath string) (restored string, err error) {
	if !IsShim(claudePath) {
		return "", ErrNotInstalled
	}
	backup := BackupPath(claudePath)
	if _, err := os.Stat(backup); err != nil {
		return "", fmt.Errorf("the shim is installed but the real binary %s is missing; delete the shim instead", backup)
	}
	if err := os.Rename(backup, claudePath); err != nil {
		return "", fmt.Errorf("restore the real claude binary: %w", err)
	}
	return claudePath, nil
}

// Plan is what `cproxy claude` should do with the real binary.
type Plan int

const (
	// PlanProxy routes claude through the shared local serve instance.
	PlanProxy Plan = iota
	// PlanPassThrough runs claude completely untouched.
	PlanPassThrough
)

// Decide picks how claude is launched and, for PlanProxy, the client token
// it presents to the shared serve instance.
//
// Pass-through (claude runs as if cproxy were not installed) when the
// environment already carries its own Anthropic routing — an
// ANTHROPIC_BASE_URL (Amp BYOK, another proxy), or an ANTHROPIC_AUTH_TOKEN
// that is not a cproxy client token — or when nothing at all is configured.
//
// The token for PlanProxy comes from, in order:
//
//  1. $CPROXY_AUTH_TOKEN — must be a configured client token with proxy
//     scope; anything else is an error, since it names cproxy explicitly
//  2. $ANTHROPIC_AUTH_TOKEN, when it matches one (so Amp project env vars
//     can drive the wrapper)
//  3. the first proxy-scoped token in $CPROXY_CLIENT_TOKENS list order
//  4. the first proxy-scoped token in credentials.json, by client name
//  5. the auto-generated local token (config.LocalClientName)
//
// Accounts without any client token yield ErrNoClientToken.
func Decide(rt *config.Runtime, environ []string) (plan Plan, reason, token string, ca config.ClientAuth, err error) {
	// Foreign routing wins: never clobber a base URL the environment
	// already carries.
	if getenv(environ, "ANTHROPIC_BASE_URL") != "" {
		return PlanPassThrough, "ANTHROPIC_BASE_URL is already set", "", config.ClientAuth{}, nil
	}
	if t := config.EnvAuthToken(environ); t != "" {
		ca, ok := rt.ClientAuth[t]
		if !ok || !ca.HasScope(config.ScopeProxy) {
			return 0, "", "", config.ClientAuth{},
				fmt.Errorf("%s is not a configured client token with proxy scope; fix it or unset it (see cproxy token, CPROXY_CLIENT_TOKENS, client_tokens in credentials.json)", config.EnvAuthTokenName)
		}
		return PlanProxy, "", t, ca, nil
	}
	if t := getenv(environ, "ANTHROPIC_AUTH_TOKEN"); t != "" {
		if ca, ok := rt.ClientAuth[t]; ok && ca.HasScope(config.ScopeProxy) {
			return PlanProxy, "", t, ca, nil
		}
		// A foreign credential: whoever set it decides where claude goes.
		return PlanPassThrough, "ANTHROPIC_AUTH_TOKEN is set and is not a cproxy client token", "", config.ClientAuth{}, nil
	}
	for _, e := range config.EnvClientTokenList(environ) {
		if ca, ok := rt.ClientAuth[e.Spec.Token]; ok && ca.HasScope(config.ScopeProxy) {
			return PlanProxy, "", e.Spec.Token, ca, nil
		}
	}
	// File-configured client tokens have no list order; pick by name. The
	// auto-generated local token is the fallback, so a token configured
	// later takes over.
	var names []string
	byName := map[string]string{}
	local := ""
	for tok, ca := range rt.ClientAuth {
		if !ca.HasScope(config.ScopeProxy) {
			continue
		}
		if ca.Name == config.LocalClientName {
			local = tok
			continue
		}
		names = append(names, ca.Name)
		byName[ca.Name] = tok
	}
	if len(names) > 0 {
		sort.Strings(names)
		tok := byName[names[0]]
		return PlanProxy, "", tok, rt.ClientAuth[tok], nil
	}
	// The local token only exists to serve pool accounts; left over after
	// the accounts are gone, it must not stop claude from passing through.
	if local != "" && len(rt.Accounts) > 0 {
		return PlanProxy, "", local, rt.ClientAuth[local], nil
	}
	if len(rt.Accounts) > 0 {
		return 0, "", "", config.ClientAuth{}, ErrNoClientToken
	}
	return PlanPassThrough, "no cproxy credentials are configured", "", config.ClientAuth{}, nil
}

// getenv reads one variable from an environ-style slice.
func getenv(environ []string, name string) string {
	for _, kv := range environ {
		if k, v, ok := cutEnv(kv); ok && k == name && v != "" {
			return v
		}
	}
	return ""
}

// cutEnv splits "NAME=value".
func cutEnv(kv string) (string, string, bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return "", "", false
}

// ServeLogPath is where the wrapper's spawned serve instance logs.
func ServeLogPath() string {
	return filepath.Join(config.Home(), "serve.log")
}
