package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/dehydr8/cproxy/internal/pool"
)

func TestClientTokenSpecUnmarshal(t *testing.T) {
	var s ClientTokenSpec
	if err := json.Unmarshal([]byte(`"cp_plain"`), &s); err != nil {
		t.Fatal(err)
	}
	if s.Token != "cp_plain" || s.Pool != "" {
		t.Fatalf("string form = %+v", s)
	}
	if err := json.Unmarshal([]byte(`{"token":"cp_obj","pool":"cvp"}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.Token != "cp_obj" || s.Pool != "cvp" {
		t.Fatalf("object form = %+v", s)
	}
	if err := json.Unmarshal([]byte(`""`), &s); err == nil {
		t.Fatal("empty string token accepted")
	}
	if err := json.Unmarshal([]byte(`{"pool":"cvp"}`), &s); err == nil {
		t.Fatal("object without token accepted")
	}
}

func TestClientTokenSpecMarshalRoundtrip(t *testing.T) {
	b, err := json.Marshal(ClientTokenSpec{Token: "cp_x"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"cp_x"` {
		t.Fatalf("plain form marshaled as %s", b)
	}
	b, err = json.Marshal(ClientTokenSpec{Token: "cp_x", Pool: "cvp"})
	if err != nil {
		t.Fatal(err)
	}
	var back ClientTokenSpec
	if err := json.Unmarshal(b, &back); err != nil || back.Pool != "cvp" {
		t.Fatalf("object roundtrip = %+v, %v", back, err)
	}
}

func TestSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvHome, dir)
	cfg := &FileConfig{
		Accounts: []pool.Account{
			{Name: "work", Token: "sk-ant-oat01-work", Type: pool.TokenTypeOAuth, Pools: []string{"cvp", DefaultPoolName}},
			{Name: "api", Token: "sk-ant-api03-key", Type: pool.TokenTypeAPI},
		},
		Pools: map[string]PoolConfig{"cvp": {Strategy: "round-robin"}},
		ClientTokens: map[string]ClientTokenSpec{
			"alice": {Token: "cp_alice", Pool: "cvp"},
			"bob":   {Token: "cp_bob"},
		},
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("credentials.json mode = %v, want 0600", fi.Mode().Perm())
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Fatalf("roundtrip mismatch:\n got %+v\nwant %+v", got, cfg)
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	cfg, err := Load()
	if err != nil || cfg == nil {
		t.Fatalf("missing file = %v, %v; want empty config", cfg, err)
	}
	if len(cfg.Accounts) != 0 {
		t.Fatalf("unexpected accounts: %v", cfg.Accounts)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		cfg  FileConfig
		want string
	}{
		{"dup account", FileConfig{Accounts: []pool.Account{{Name: "a", Token: "t"}, {Name: "a", Token: "t"}}}, "duplicate account"},
		{"empty token", FileConfig{Accounts: []pool.Account{{Name: "a"}}}, "empty token"},
		{"bad strategy", FileConfig{Pools: map[string]PoolConfig{"p": {Strategy: "fastest"}}}, "unknown strategy"},
		{"dup client secret", FileConfig{ClientTokens: map[string]ClientTokenSpec{"a": {Token: "cp_x"}, "b": {Token: "cp_x"}}}, "share the same secret"},
	}
	for _, tc := range cases {
		err := tc.cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want containing %q", tc.name, err, tc.want)
		}
	}
}

func TestParseTokenList(t *testing.T) {
	got := parseTokenList("main:tok1, alt:tok2", "env")
	if len(got) != 2 || got[0].Name != "main" || got[1].Name != "alt" {
		t.Fatalf("named parse = %+v", got)
	}
	got = parseTokenList("tok1, tok2\n, tok3", "env")
	if len(got) != 3 || got[0].Name != "env-1" || got[2].Token != "tok3" {
		t.Fatalf("bare parse = %+v", got)
	}
	got = parseTokenList("a:tok1,a:tok2", "env")
	if len(got) != 1 || got[0].Token != "tok2" {
		t.Fatalf("duplicate name did not collapse to last: %+v", got)
	}
	if got := parseTokenList(",, \n", "env"); got != nil {
		t.Fatalf("empty list = %+v", got)
	}
}

func TestEnvAccounts(t *testing.T) {
	environ := []string{
		"CPROXY_TOKENS=main:sk-ant-oat01-x,sk-ant-api03-y",
		"CPROXY_POOL_TOKENS_CVP=lead:sk-ant-oat01-1,alt:sk-ant-oat01-2",
		"CPROXY_UPSTREAM=https://example.invalid",
	}
	got, err := envAccounts(environ)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]pool.Account{}
	for _, a := range got {
		byName[a.Name] = a
	}
	if a := byName["main"]; a.Pools[0] != DefaultPoolName || a.Type != pool.TokenTypeOAuth || a.Source != "env" {
		t.Fatalf("main = %+v", a)
	}
	if a := byName["env-1"]; a.Type != pool.TokenTypeAPI {
		t.Fatalf("bare token not auto-typed: %+v", a)
	}
	if a := byName["lead"]; a.Pools[0] != "cvp" {
		t.Fatalf("cvp member pools = %v", a.Pools)
	}
	if a := byName["alt"]; a.Pools[0] != "cvp" {
		t.Fatalf("cvp member pools = %v", a.Pools)
	}
}

func TestEnvPerAccountTokens(t *testing.T) {
	environ := []string{
		"CPROXY_TOKEN_ABC=sk-ant-oat01-abc",
		"CPROXY_TOKEN_XYZ=sk-ant-api03-xyz",
		"CPROXY_POOL_TOKEN_CVP_A=sk-ant-oat01-a",
		"CPROXY_POOL_TOKEN_CVP_B=sk-ant-oat01-b",
		// Same name in another pool must not collide with cvp's "a".
		"CPROXY_POOL_TOKEN_OPS_A=sk-ant-oat01-opsa",
		// The same subscription in two pools is one account in both.
		"CPROXY_POOL_TOKEN_OPS_SHARED=sk-ant-oat01-abc",
		"CPROXY_POOL=cvp",
	}
	got, err := envAccounts(environ)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]pool.Account{}
	for _, a := range got {
		byName[a.Name] = a
	}
	want := map[string][]string{
		"abc":   {DefaultPoolName, "ops"},
		"xyz":   {DefaultPoolName},
		"cvp-a": {"cvp"},
		"cvp-b": {"cvp"},
		"ops-a": {"ops"},
	}
	if len(got) != len(want) {
		t.Fatalf("accounts = %+v; want exactly %v", got, want)
	}
	for name, pools := range want {
		a, ok := byName[name]
		if !ok || !slices.Equal(a.Pools, pools) {
			t.Fatalf("account %q = %+v; want pools %v", name, a, pools)
		}
	}
	if byName["xyz"].Type != pool.TokenTypeAPI || byName["cvp-a"].Token != "sk-ant-oat01-a" {
		t.Fatalf("token/type mixed up: %+v", got)
	}

	// CPROXY_POOL picks the pool clients without their own use.
	rt, err := Build(&FileConfig{}, environ)
	if err != nil {
		t.Fatal(err)
	}
	members, _, err := rt.ResolvePool("")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range members {
		names = append(names, m.Name)
	}
	if !slices.Equal(names, []string{"cvp-a", "cvp-b"}) || rt.PoolFor(ClientAuth{Name: "x"}) != "cvp" {
		t.Fatalf("CPROXY_POOL=cvp resolved to %v", names)
	}
	if rt.PoolFor(ClientAuth{Name: "x", Pool: "ops"}) != "ops" {
		t.Fatal("a client's own pool must beat CPROXY_POOL")
	}

	if _, err := envAccounts([]string{"CPROXY_POOL_TOKEN_CVP=sk-ant-oat01-a"}); err == nil {
		t.Fatal("CPROXY_POOL_TOKEN_<POOL> without an account name was accepted")
	}
}

func TestBuildMergesEnvOverFile(t *testing.T) {
	file := &FileConfig{
		Accounts: []pool.Account{{Name: "main", Token: "file-tok", Pools: []string{"cvp"}}},
		ClientTokens: map[string]ClientTokenSpec{
			"alice": {Token: "cp_file_alice"},
		},
	}
	environ := []string{
		"CPROXY_TOKENS=main:env-tok,fresh:env-tok2",
		"CPROXY_CLIENT_TOKENS=alice:cp_env_alice,bob:cp_bob",
	}
	rt, err := Build(file, environ)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]pool.Account{}
	for _, a := range rt.Accounts {
		byName[a.Name] = a
	}
	if a := byName["main"]; a.Token != "env-tok" {
		t.Fatalf("env did not override file token: %+v", a)
	}
	if a := byName["main"]; a.Pools[0] != DefaultPoolName {
		t.Fatalf("env account pools = %v, want [default]", a.Pools)
	}
	if _, ok := byName["fresh"]; !ok {
		t.Fatalf("env-only account missing: %+v", rt.Accounts)
	}
	if ca, ok := rt.ClientAuth["cp_env_alice"]; !ok || ca.Name != "alice" {
		t.Fatalf("env client token missing: %+v", rt.ClientAuth)
	}
	if _, ok := rt.ClientAuth["cp_file_alice"]; ok {
		t.Fatal("superseded file client token still present")
	}
	if ca, ok := rt.ClientAuth["cp_bob"]; !ok || ca.Name != "bob" {
		t.Fatalf("bob missing: %+v", rt.ClientAuth)
	}
}

func TestBuildDetectsMissingType(t *testing.T) {
	// A hand-edited file may omit "type": the API key must still go out as
	// x-api-key, not as a bearer token.
	file := &FileConfig{Accounts: []pool.Account{
		{Name: "key", Token: "sk-ant-api03-x"},
		{Name: "sub", Token: "sk-ant-oat01-x"},
		{Name: "forced", Token: "sk-ant-api03-y", Type: pool.TokenTypeOAuth},
	}}
	rt, err := Build(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]pool.TokenType{"key": pool.TokenTypeAPI, "sub": pool.TokenTypeOAuth, "forced": pool.TokenTypeOAuth}
	for _, a := range rt.Accounts {
		if a.Type != want[a.Name] {
			t.Fatalf("account %q type = %q, want %q", a.Name, a.Type, want[a.Name])
		}
	}
}

func TestResolvePool(t *testing.T) {
	rt := &Runtime{
		Accounts: []pool.Account{
			{Name: "plain", Token: "t1"},
			{Name: "cvp-only", Token: "t2", Pools: []string{"cvp"}},
			{Name: "both", Token: "t3", Pools: []string{"cvp", DefaultPoolName}},
			{Name: "off", Token: "t4"},
		},
		Pools:       map[string]PoolConfig{"cvp": {Strategy: "least-used"}},
		ClientAuth:  map[string]ClientAuth{},
		DefaultPool: DefaultPoolName,
	}
	rt.Accounts[3].Disabled = true

	members, strategy, err := rt.ResolvePool("")
	if err != nil || len(members) != 2 || members[0].Name != "plain" || strategy != pool.DefaultStrategy {
		t.Fatalf("default pool = %v %q %v", members, strategy, err)
	}
	members, strategy, err = rt.ResolvePool("cvp")
	if err != nil || len(members) != 2 || strategy != pool.StrategyLeastUsed {
		t.Fatalf("cvp pool = %v %q %v", members, strategy, err)
	}
	_, _, err = rt.ResolvePool("nope")
	if err == nil || !strings.Contains(err.Error(), "cvp") {
		t.Fatalf("unknown pool err = %v", err)
	}

	rt2 := &Runtime{
		Accounts:    []pool.Account{{Name: "off", Token: "t", Pools: []string{"solo"}}},
		ClientAuth:  map[string]ClientAuth{},
		DefaultPool: DefaultPoolName,
	}
	rt2.Accounts[0].Disabled = true
	if _, _, err := rt2.ResolvePool("solo"); err == nil || !strings.Contains(err.Error(), "no usable members") {
		t.Fatalf("all-disabled err = %v", err)
	}
}

func TestPoolNames(t *testing.T) {
	rt := &Runtime{
		Accounts: []pool.Account{
			{Name: "a", Token: "t"},
			{Name: "b", Token: "t", Pools: []string{"cvp"}},
		},
		Pools:       map[string]PoolConfig{"extra": {}},
		ClientAuth:  map[string]ClientAuth{},
		DefaultPool: DefaultPoolName,
	}
	got := strings.Join(rt.PoolNames(), ",")
	if got != "cvp,default,extra" {
		t.Fatalf("pool names = %q", got)
	}
}

func TestEnvUpstream(t *testing.T) {
	if got := EnvUpstream(nil); got != "" {
		t.Fatalf("empty environ = %q", got)
	}
	if got := EnvUpstream([]string{"CPROXY_UPSTREAM=https://x.example", "OTHER=1"}); got != "https://x.example" {
		t.Fatalf("env upstream = %q", got)
	}
}

func TestHomeOverride(t *testing.T) {
	t.Setenv(EnvHome, "/custom")
	if got := Home(); got != "/custom" {
		t.Fatalf("home = %q", got)
	}
	if got := Path(); got != filepath.Join("/custom", "credentials.json") {
		t.Fatalf("path = %q", got)
	}
}

func TestSaveRejectsInvalid(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	err := Save(&FileConfig{Accounts: []pool.Account{{Name: ""}}})
	if err == nil || !errors.Is(err, err) {
		t.Fatalf("invalid save err = %v", err)
	}
	if _, serr := os.Stat(Path()); !os.IsNotExist(serr) {
		t.Fatalf("invalid config was written: %v", serr)
	}
}

func TestClientTokenScopes(t *testing.T) {
	file := &FileConfig{
		Accounts: []pool.Account{{Name: "a", Token: "tokA"}},
		ClientTokens: map[string]ClientTokenSpec{
			"alice":   {Token: "cp_alice"}, // no scopes: full access
			"monitor": {Token: "cp_monitor", Scopes: []string{"usage"}},
			"worker":  {Token: "cp_worker", Scopes: []string{"proxy"}},
		},
	}
	rt, err := Build(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ca := rt.ClientAuth["cp_alice"]; !ca.HasScope(ScopeProxy) || !ca.HasScope(ScopeUsage) {
		t.Fatalf("scopeless token should have full access: %+v", ca)
	}
	if ca := rt.ClientAuth["cp_monitor"]; ca.HasScope(ScopeProxy) || !ca.HasScope(ScopeUsage) {
		t.Fatalf("usage-scoped token: %+v", ca)
	}
	if ca := rt.ClientAuth["cp_worker"]; !ca.HasScope(ScopeProxy) || ca.HasScope(ScopeUsage) {
		t.Fatalf("proxy-scoped token: %+v", ca)
	}

	// Env form: "name:token:scopes" with "+"-separated scopes.
	env := []string{EnvClientTokens + "=mon2:cp_mon2:usage,al2:cp_al2:proxy+usage"}
	rt, err = Build(&FileConfig{Accounts: file.Accounts}, env)
	if err != nil {
		t.Fatal(err)
	}
	if ca := rt.ClientAuth["cp_mon2"]; ca.HasScope(ScopeProxy) || !ca.HasScope(ScopeUsage) {
		t.Fatalf("env usage-scoped token: %+v", ca)
	}
	if ca := rt.ClientAuth["cp_al2"]; !ca.HasScope(ScopeProxy) || !ca.HasScope(ScopeUsage) {
		t.Fatalf("env both-scopes token: %+v", ca)
	}

	// Unknown scopes are rejected from both sources.
	if _, err := Build(&FileConfig{ClientTokens: map[string]ClientTokenSpec{
		"x": {Token: "cp_x", Scopes: []string{"admin"}},
	}}, nil); err == nil {
		t.Fatal("invalid file scope accepted")
	}
	if _, err := Build(&FileConfig{}, []string{EnvClientTokens + "=x:cp_x:admin"}); err == nil {
		t.Fatal("invalid env scope accepted")
	}

	// Scopes survive the JSON roundtrip.
	b, err := json.Marshal(ClientTokenSpec{Token: "cp_x", Scopes: []string{"usage"}})
	if err != nil {
		t.Fatal(err)
	}
	var back ClientTokenSpec
	if err := json.Unmarshal(b, &back); err != nil || len(back.Scopes) != 1 || back.Scopes[0] != "usage" {
		t.Fatalf("scope roundtrip = %s, %+v, %v", b, back, err)
	}
}

func TestClientTokenListOrder(t *testing.T) {
	got := ClientTokenList("monitor:cp_m:usage,amp:cp_a,bare:cp_b:proxy+usage")
	want := []NamedClientToken{
		{Name: "monitor", Spec: ClientTokenSpec{Token: "cp_m", Scopes: []string{"usage"}}},
		{Name: "amp", Spec: ClientTokenSpec{Token: "cp_a"}},
		{Name: "bare", Spec: ClientTokenSpec{Token: "cp_b", Scopes: []string{"proxy", "usage"}}},
	}
	if len(got) != len(want) {
		t.Fatalf("ClientTokenList = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].Spec.Token != want[i].Spec.Token ||
			strings.Join(got[i].Spec.Scopes, "+") != strings.Join(want[i].Spec.Scopes, "+") {
			t.Fatalf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(ClientTokenList("nope:")) != 0 {
		t.Fatal("empty tokens accepted")
	}
}

func TestEnvHelpers(t *testing.T) {
	env := []string{
		EnvClientTokens + "=amp:cp_a,monitor:cp_m:usage",
		EnvAuthTokenName + "=cp_x",
		EnvPortName + "=7790",
		"OTHER=1",
	}
	if got := EnvClientTokenList(env); len(got) != 2 || got[0].Name != "amp" || got[1].Name != "monitor" {
		t.Fatalf("EnvClientTokenList = %+v, want amp then monitor", got)
	}
	if got := EnvClientTokenList(nil); got != nil {
		t.Fatalf("EnvClientTokenList(nil) = %+v", got)
	}
	if got := EnvAuthToken(env); got != "cp_x" {
		t.Fatalf("EnvAuthToken = %q", got)
	}
	if got := EnvPort(env); got != "7790" {
		t.Fatalf("EnvPort = %q", got)
	}
}

func TestLocalToken(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())

	// Concurrent first launches must all agree on one token.
	toks := make([]string, 8)
	var wg sync.WaitGroup
	for i := range toks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := EnsureLocalToken()
			if err != nil {
				t.Error(err)
			}
			toks[i] = tok
		}()
	}
	wg.Wait()
	for _, tok := range toks {
		if !strings.HasPrefix(tok, "cp_") || tok != toks[0] {
			t.Fatalf("local tokens = %q; want one shared cp_ token", toks)
		}
	}
	if info, err := os.Stat(LocalTokenPath()); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("local token file: %v %v", err, info.Mode())
	}

	// Load + Build register it with full access; Save never persists it.
	file, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	rt, err := Build(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	ca, ok := rt.ClientAuth[toks[0]]
	if !ok || ca.Name != LocalClientName || !ca.HasScope(ScopeProxy) || !ca.HasScope(ScopeUsage) {
		t.Fatalf("local client = %+v (%v)", ca, ok)
	}
	file.Accounts = []pool.Account{{Name: "a", Token: "sk-ant-oat01-a"}}
	if err := Save(file); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(Path()); strings.Contains(string(b), toks[0]) {
		t.Fatalf("local token leaked into credentials.json:\n%s", b)
	}
}
