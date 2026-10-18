package daemon

import "testing"

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"v1.6.0", "v1.5.0", true},
		{"v1.10.0", "v1.9.9", true}, // numeric, not lexical
		{"v1.5.0", "v1.5.0", false},
		{"v1.4.9", "v1.5.0", false},
		{"dev+abc", "v1.5.0", false}, // development builds never take over
		{"v1.5.0", "dev+abc", false},
	} {
		if got := Newer(tc.a, tc.b); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestConfigEnv(t *testing.T) {
	env := []string{"CPROXY_TOKEN_A=x", "HOME=/h", "CPROXY_HOME=/c", "CPROXY_PORT=1", "CPROXY_POOL=cvp"}
	got := ConfigEnv(env)
	if len(got) != 2 || got[0] != "CPROXY_POOL=cvp" || got[1] != "CPROXY_TOKEN_A=x" {
		t.Fatalf("ConfigEnv = %v", got)
	}
	if EnvHash(env) != EnvHash(append([]string{"CPROXY_PORT=2", "PATH=/x"}, env...)) {
		t.Fatal("hash depends on variables that do not configure the daemon")
	}
}
