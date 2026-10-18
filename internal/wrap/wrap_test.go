package wrap

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dehydr8/cproxy/internal/config"
	"github.com/dehydr8/cproxy/internal/pool"
)

func TestShimScript(t *testing.T) {
	got := ShimScript("/usr/local/bin/cproxy", "/home/u/.local/bin/claude.cproxy-real")
	for _, want := range []string{
		"#!/bin/sh\n",
		Marker,
		`exec '/usr/local/bin/cproxy' claude --exec '/home/u/.local/bin/claude.cproxy-real' -- "$@"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("shim script missing %q:\n%s", want, got)
		}
	}
}

func TestShimScriptOddPaths(t *testing.T) {
	// The shim must hand both paths and claude's args to exec verbatim,
	// whatever characters they contain.
	dir := filepath.Join(t.TempDir(), `it's "a" $HOME `+"`x`"+` dir`)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "cproxy")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "claude.cproxy-real")
	shim := filepath.Join(dir, "claude")
	if err := os.WriteFile(shim, []byte(ShimScript(fake, real)), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(shim, "-p", "a b").Output()
	if err != nil {
		t.Fatalf("shim failed: %v", err)
	}
	want := strings.Join([]string{"claude", "--exec", real, "--", "-p", "a b"}, "\n") + "\n"
	if string(out) != want {
		t.Fatalf("shim passed:\n%s\nwant:\n%s", out, want)
	}
}

func TestInstallUninstallRoundTrip(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\nreal claude\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	shim, backup, err := Install(claude)
	if err != nil {
		t.Fatal(err)
	}
	if shim != claude || backup != BackupPath(claude) {
		t.Fatalf("install paths: shim=%s backup=%s", shim, backup)
	}
	if !IsShim(claude) {
		t.Fatal("claude path does not hold the shim after install")
	}
	restored, err := os.ReadFile(backup)
	if err != nil || string(restored) != "#!/bin/sh\nreal claude\n" {
		t.Fatalf("backup does not hold the real binary: %v %q", err, restored)
	}
	if info, err := os.Stat(claude); err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("shim is not executable: %v %v", err, info.Mode())
	}

	// Re-installing a shim is a no-op sentinel.
	if _, _, err := Install(claude); !errors.Is(err, ErrAlreadyInstalled) {
		t.Fatalf("install over shim = %v, want ErrAlreadyInstalled", err)
	}

	if restored, err := Uninstall(claude); err != nil || restored != claude {
		t.Fatalf("uninstall: %v %s", err, restored)
	}
	body, err := os.ReadFile(claude)
	if err != nil || string(body) != "#!/bin/sh\nreal claude\n" {
		t.Fatalf("claude not restored: %v %q", err, body)
	}
	if _, err := os.Stat(BackupPath(claude)); !os.IsNotExist(err) {
		t.Fatal("backup still present after uninstall")
	}
	if IsShim(claude) {
		t.Fatal("restored binary detected as shim")
	}
	if _, err := Uninstall(claude); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("uninstall of plain binary = %v, want ErrNotInstalled", err)
	}
}

func TestUninstallMissingBackup(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude")
	if err := os.WriteFile(claude, []byte(ShimScript("cproxy", "nowhere")), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(claude); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("uninstall without backup = %v, want missing-binary error", err)
	}
}

func TestIsShimIgnoresPlainScripts(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "claude")
	if err := os.WriteFile(plain, []byte("#!/bin/sh\nexec something\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if IsShim(plain) {
		t.Fatal("plain script detected as shim")
	}
	if IsShim(filepath.Join(dir, "nope")) {
		t.Fatal("missing file detected as shim")
	}
}

func TestDecide(t *testing.T) {
	accounts := []pool.Account{{Name: "a", Token: "sk-ant-a"}}
	rt := &config.Runtime{
		Accounts: accounts,
		ClientAuth: map[string]config.ClientAuth{
			"cp_amp":     {Name: "amp"},
			"cp_monitor": {Name: "monitor", Scopes: []string{config.ScopeUsage}},
			"cp_zeta":    {Name: "zeta"},
			"cp_alpha":   {Name: "alpha"},
		},
	}
	envClientTokens := []string{config.EnvClientTokens + "=amp:cp_amp,monitor:cp_monitor:usage"}

	tests := []struct {
		name      string
		rt        *config.Runtime
		environ   []string
		wantPlan  Plan
		wantToken string
		wantErr   bool
	}{
		{
			name:     "foreign base URL wins",
			rt:       rt,
			environ:  append([]string{"ANTHROPIC_BASE_URL=https://other"}, envClientTokens...),
			wantPlan: PlanPassThrough,
		},
		{
			name:      "explicit cproxy auth token",
			rt:        rt,
			environ:   envClientTokens,
			wantToken: "cp_amp",
			wantPlan:  PlanProxy,
		},
		{
			name:    "explicit cproxy auth token that is unknown is an error",
			rt:      rt,
			environ: []string{config.EnvAuthTokenName + "=cp_nope"},
			wantErr: true,
		},
		{
			name:    "explicit cproxy auth token without proxy scope is an error",
			rt:      rt,
			environ: []string{config.EnvAuthTokenName + "=cp_monitor"},
			wantErr: true,
		},
		{
			name:      "matching anthropic auth token",
			rt:        rt,
			environ:   []string{"ANTHROPIC_AUTH_TOKEN=cp_amp"},
			wantToken: "cp_amp",
			wantPlan:  PlanProxy,
		},
		{
			name:     "foreign anthropic auth token passes through",
			rt:       rt,
			environ:  []string{"ANTHROPIC_AUTH_TOKEN=sk-ant-not-cproxy"},
			wantPlan: PlanPassThrough,
		},
		{
			name:      "first proxy-scoped token in list order",
			rt:        rt,
			environ:   envClientTokens,
			wantToken: "cp_amp",
			wantPlan:  PlanProxy,
		},
		{
			name:      "usage-scoped tokens are skipped in the list",
			rt:        rt,
			environ:   []string{config.EnvClientTokens + "=monitor:cp_monitor:usage,zeta:cp_zeta"},
			wantToken: "cp_zeta",
			wantPlan:  PlanProxy,
		},
		{
			name:      "file-only tokens pick by client name",
			rt:        rt,
			wantToken: "cp_alpha",
			wantPlan:  PlanProxy,
		},
		{
			name:    "accounts without any client token are an error",
			rt:      &config.Runtime{Accounts: accounts},
			wantErr: true,
		},
		{
			name:     "nothing configured passes through",
			rt:       &config.Runtime{},
			environ:  nil,
			wantPlan: PlanPassThrough,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, _, token, _, err := Decide(tt.rt, tt.environ)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Decide = %v, want error", plan)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if plan != tt.wantPlan {
				t.Fatalf("plan = %v, want %v", plan, tt.wantPlan)
			}
			if token != tt.wantToken {
				t.Fatalf("token = %q, want %q", token, tt.wantToken)
			}
		})
	}
}
