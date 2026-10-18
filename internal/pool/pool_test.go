package pool

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func accounts(names ...string) []Account {
	var out []Account
	for _, n := range names {
		out = append(out, Account{Name: n, Token: "sk-ant-oat01-" + n})
	}
	return out
}

func head(t *testing.T, s *Selector, m []Account, strategy Strategy, pool, client string) Account {
	t.Helper()
	got := s.Ranked(m, strategy, pool, client)
	if len(got) == 0 {
		t.Fatalf("ranked returned no accounts for client %q", client)
	}
	return got[0]
}

func TestRankedStickyPinsClient(t *testing.T) {
	s := NewSelector()
	m := accounts("a", "b", "c")
	if got := head(t, s, m, StrategySticky, "p", "c1"); got.Name != "a" {
		t.Fatalf("first assignment = %q, want a", got.Name)
	}
	for i := 0; i < 3; i++ {
		if got := head(t, s, m, StrategySticky, "p", "c1"); got.Name != "a" {
			t.Fatalf("sticky client moved to %q on request %d", got.Name, i+2)
		}
	}
}

func TestStickySpreadsNewClients(t *testing.T) {
	s := NewSelector()
	m := accounts("a", "b", "c")
	for i, want := range []string{"a", "b", "c"} {
		client := "c" + string(rune('0'+i+1))
		if got := head(t, s, m, StrategySticky, "p", client); got.Name != want {
			t.Fatalf("new client %d assigned %q, want %q", i+1, got.Name, want)
		}
	}
}

func TestStickyRemapsWhenPinnedAccountLimited(t *testing.T) {
	s := NewSelector()
	m := accounts("a", "b")
	if got := head(t, s, m, StrategySticky, "p", "c"); got.Name != "a" {
		t.Fatalf("pinned = %q, want a", got.Name)
	}
	s.MarkRateLimited("a", time.Now().Add(20*time.Millisecond))
	if got := head(t, s, m, StrategySticky, "p", "c"); got.Name != "b" {
		t.Fatalf("did not remap after cooldown: %q", got.Name)
	}
	// a recovers, but the client must stay on b (no bounce-back).
	time.Sleep(30 * time.Millisecond)
	if got := head(t, s, m, StrategySticky, "p", "c"); got.Name != "b" {
		t.Fatalf("client bounced back to recovered account: %q", got.Name)
	}
}

func TestStickyRemapsWhenPinnedAccountRejected(t *testing.T) {
	s := NewSelector()
	m := accounts("a", "b")
	head(t, s, m, StrategySticky, "p", "c") // pins a
	s.MarkInvalid("a")
	if got := head(t, s, m, StrategySticky, "p", "c"); got.Name != "b" {
		t.Fatalf("did not remap after invalid: %q", got.Name)
	}
	s.ClearAccount("a")
	// Invalid state is cleared, but the client stays on b.
	if got := head(t, s, m, StrategySticky, "p", "c"); got.Name != "b" {
		t.Fatalf("client moved after clear: %q", got.Name)
	}
}

func TestRankedAllUnavailable(t *testing.T) {
	s := NewSelector()
	m := accounts("a", "b")
	s.MarkRateLimited("a", time.Now().Add(time.Hour))
	s.MarkRateLimited("b", time.Now().Add(time.Hour))
	if got := s.Ranked(m, StrategySticky, "p", "c"); got != nil {
		t.Fatalf("ranked returned %v, want nil", got)
	}
}

func TestRoundRobinRotatesPerRequest(t *testing.T) {
	s := NewSelector()
	m := accounts("a", "b", "c")
	for i, want := range []string{"a", "b", "c", "a"} {
		if got := head(t, s, m, StrategyRoundRobin, "p", "c"); got.Name != want {
			t.Fatalf("request %d head = %q, want %q", i+1, got.Name, want)
		}
	}
}

func TestLeastUsedPrefersFewestRequests(t *testing.T) {
	s := NewSelector()
	m := accounts("a", "b")
	s.MarkSuccess("b")
	s.MarkSuccess("b")
	if got := head(t, s, m, StrategyLeastUsed, "p", "c"); got.Name != "a" {
		t.Fatalf("least-used head = %q, want a", got.Name)
	}
}

func TestRandomReturnsPermutation(t *testing.T) {
	s := NewSelector()
	m := accounts("a", "b", "c")
	want := []string{"a", "b", "c"}
	for i := 0; i < 25; i++ {
		got := s.Ranked(m, StrategyRandom, "p", "c")
		names := make([]string, len(got))
		for j, a := range got {
			names[j] = a.Name
		}
		slices.Sort(names)
		if strings.Join(names, ",") != strings.Join(want, ",") {
			t.Fatalf("random ranking %d = %v, want a permutation of %v", i, names, want)
		}
	}
}

func TestDisabledExcluded(t *testing.T) {
	s := NewSelector()
	m := []Account{{Name: "a", Token: "t", Disabled: true}, {Name: "b", Token: "t"}}
	got := s.Ranked(m, StrategySticky, "p", "c")
	if len(got) != 1 || got[0].Name != "b" {
		t.Fatalf("ranked = %v, want [b]", got)
	}
}

func TestNextRecoveryEarliest(t *testing.T) {
	s := NewSelector()
	m := accounts("a", "b")
	s.MarkRateLimited("a", time.Now().Add(2*time.Hour))
	s.MarkRateLimited("b", time.Now().Add(20*time.Millisecond))
	got := s.NextRecovery(m)
	if got.IsZero() {
		t.Fatal("next recovery is zero")
	}
	if d := time.Until(got); d > 25*time.Millisecond || d < 0 {
		t.Fatalf("next recovery = %v, want ~20ms", d)
	}
	time.Sleep(30 * time.Millisecond) // b's cooldown expires
	if d := time.Until(s.NextRecovery(m)); d < 59*time.Minute {
		t.Fatalf("expired cooldown still counted: %v", d)
	}
}

func TestMarkRateLimitedKeepsLonger(t *testing.T) {
	s := NewSelector()
	long := time.Now().Add(time.Hour)
	s.MarkRateLimited("a", long)
	s.MarkRateLimited("a", time.Now().Add(time.Minute))
	if got := s.NextRecovery(accounts("a")); !got.Equal(long) {
		t.Fatalf("cooldown was shortened: %v, want %v", got, long)
	}
}

func TestParseStrategy(t *testing.T) {
	if v, err := ParseStrategy(""); err != nil || v != DefaultStrategy {
		t.Fatalf("empty strategy = %q, %v; want default sticky", v, err)
	}
	for _, s := range allStrategies {
		if _, err := ParseStrategy(s); err != nil {
			t.Fatalf("ParseStrategy(%q): %v", s, err)
		}
	}
	if _, err := ParseStrategy("fastest"); err == nil {
		t.Fatal("unknown strategy accepted")
	}
}

func TestDetectTokenType(t *testing.T) {
	if got := DetectTokenType("sk-ant-api03-xyz"); got != TokenTypeAPI {
		t.Fatalf("api token detected as %q", got)
	}
	if got := DetectTokenType("sk-ant-oat01-xyz"); got != TokenTypeOAuth {
		t.Fatalf("oauth token detected as %q", got)
	}
	if got := DetectTokenType("whatever"); got != TokenTypeOAuth {
		t.Fatalf("unknown token detected as %q", got)
	}
}

func TestMaskToken(t *testing.T) {
	got := MaskToken("sk-ant-oat01-abcdefgh")
	if strings.Contains(got, "abcdefgh") || !strings.Contains(got, "…") {
		t.Fatalf("mask leaked the secret: %q", got)
	}
	if got := MaskToken("short"); got == "short" {
		t.Fatalf("short token not masked: %q", got)
	}
}

func TestStatsSnapshots(t *testing.T) {
	now := time.Now()
	sel := NewSelector()
	sel.MarkSuccess("a")
	sel.MarkRateLimited("b", now.Add(time.Minute))
	sel.MarkError("c", now.Add(15*time.Second))
	sel.MarkInvalid("d")
	sel.MarkClientAttempt("alice")
	sel.MarkClientAttempt("alice")

	stats := sel.StatsSnapshot([]string{"a", "b", "c", "d", "e"})
	if stats["a"].Requests != 1 || stats["a"].LastUsed.IsZero() {
		t.Fatalf("a stats = %+v", stats["a"])
	}
	if stats["b"].RateLimited != 1 || !stats["b"].CoolingUntil.After(now.Add(30*time.Second)) {
		t.Fatalf("b stats = %+v", stats["b"])
	}
	if stats["c"].Errors != 1 || !stats["c"].CoolingUntil.After(now) {
		t.Fatalf("c stats = %+v", stats["c"])
	}
	if stats["d"].Rejected != 1 || !stats["d"].Invalid {
		t.Fatalf("d stats = %+v", stats["d"])
	}
	if stats["e"].Requests != 0 || !stats["e"].LastUsed.IsZero() {
		t.Fatalf("idle account should be zero-valued: %+v", stats["e"])
	}

	cs := sel.ClientStatsSnapshot()
	if cs["alice"].Requests != 2 || cs["alice"].LastSeen.IsZero() {
		t.Fatalf("alice client stats = %+v", cs["alice"])
	}

	// StickySnapshot is a copy: mutating it must not affect the selector.
	sel.Ranked(accounts("a"), StrategySticky, "p", "alice")
	snap := sel.StickySnapshot()
	if snap["p|alice"] != "a" {
		t.Fatalf("sticky snapshot = %v", snap)
	}
	snap["p|alice"] = "hacked"
	if got := sel.Assignment("p", "alice"); got != "a" {
		t.Fatalf("sticky snapshot is not a copy: assignment = %q", got)
	}
}

func TestRateLimitStats(t *testing.T) {
	sel := NewSelector()

	if got := sel.StatsSnapshot([]string{"a"})["a"].RateLimits; got != nil {
		t.Fatalf("rate limits before any response: %+v", got)
	}

	limit, rem := int64(50), int64(42)
	sel.MarkRateLimits("a", RateLimitInfo{
		Status:            "allowed",
		RequestsLimit:     &limit,
		RequestsRemaining: &rem,
	})

	stats := sel.StatsSnapshot([]string{"a", "b"})
	if stats["b"].RateLimits != nil {
		t.Fatalf("account b should have no rate limits: %+v", stats["b"].RateLimits)
	}
	rl := stats["a"].RateLimits
	if rl == nil || rl.Status != "allowed" || rl.RequestsRemaining == nil || *rl.RequestsRemaining != 42 {
		t.Fatalf("rate limits: %+v", rl)
	}

	// ClearAccount (reload with a changed token) drops stale rate-limit data too.
	sel.ClearAccount("a")
	if got := sel.StatsSnapshot([]string{"a"})["a"].RateLimits; got != nil {
		t.Fatalf("rate limits survived ClearAccount: %+v", got)
	}
}

// seen records a response's subscription windows for an account, as the
// proxy does from anthropic-ratelimit-unified-* headers.
func seen(s *Selector, name string, fiveHour, sevenDay float64) {
	s.MarkRateLimits(name, RateLimitInfo{FiveHourPct: &fiveHour, SevenDayPct: &sevenDay, Seen: time.Now()})
}

func TestStickyMovesBeforeTheLimit(t *testing.T) {
	s := NewSelector() // DefaultSwitchPolicy: 90%, 10 points, 5m
	m := accounts("a", "b", "c")
	if got := head(t, s, m, StrategySticky, "p", "c1"); got.Name != "a" {
		t.Fatalf("first pin = %q, want a", got.Name)
	}
	seen(s, "a", 40, 89) // binding window is the 7-day one
	seen(s, "b", 70, 30)
	seen(s, "c", 20, 25)
	if got := head(t, s, m, StrategySticky, "p", "c1"); got.Name != "a" {
		t.Fatalf("moved at 89%%: %q", got.Name)
	}
	seen(s, "a", 40, 91)
	// c has the most headroom (binding 25%) though b's 7-day is lower.
	if got := head(t, s, m, StrategySticky, "p", "c1"); got.Name != "c" {
		t.Fatalf("at 91%% moved to %q, want c (most headroom)", got.Name)
	}
	if why := s.SwitchReason("p", "c1"); !strings.Contains(why, "a at 91%") || !strings.Contains(why, "c at 25%") {
		t.Fatalf("switch reason = %q", why)
	}
	// Within the cooldown it stays put even if c fills up too.
	seen(s, "c", 95, 95)
	if got := head(t, s, m, StrategySticky, "p", "c1"); got.Name != "c" {
		t.Fatalf("moved again within the cooldown: %q", got.Name)
	}
}

func TestStickyHysteresisAndRollover(t *testing.T) {
	s := NewSelector()
	m := accounts("a", "b")
	head(t, s, m, StrategySticky, "p", "c1") // pins a
	// b is under the threshold but not 10 points better than a: stay.
	seen(s, "a", 92, 10)
	seen(s, "b", 85, 10)
	if got := head(t, s, m, StrategySticky, "p", "c1"); got.Name != "a" {
		t.Fatalf("moved to %q, only 7 points better", got.Name)
	}
	// A window whose reset has passed counts as empty.
	past := time.Now().Add(-time.Minute)
	ninety := 95.0
	s.MarkRateLimits("a", RateLimitInfo{FiveHourPct: &ninety, FiveHourReset: &past, Seen: time.Now()})
	if got := head(t, s, m, StrategySticky, "p", "c1"); got.Name != "a" {
		t.Fatalf("moved off a rolled-over window: %q", got.Name)
	}

	s.SetSwitchPolicy(SwitchPolicy{}) // threshold 0: never move proactively
	seen(s, "a", 99, 99)
	seen(s, "b", 1, 1)
	if got := head(t, s, m, StrategySticky, "p", "c1"); got.Name != "a" {
		t.Fatalf("moved with proactive switching off: %q", got.Name)
	}
}

func TestStickyNewPinsTakeMostHeadroom(t *testing.T) {
	s := NewSelector()
	m := accounts("a", "b", "c")
	seen(s, "a", 60, 60)
	seen(s, "b", 10, 5)
	seen(s, "c", 30, 30)
	if got := head(t, s, m, StrategySticky, "p", "new"); got.Name != "b" {
		t.Fatalf("new client pinned to %q, want b (most headroom)", got.Name)
	}
}
