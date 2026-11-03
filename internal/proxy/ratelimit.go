package proxy

import (
	"bytes"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Cooldowns after a 429.
const (
	// exhaustedCooldown applies when a limit reports itself exhausted but
	// sends no reset time.
	exhaustedCooldown = 10 * time.Minute
	// briefCooldown applies to any other 429 without Retry-After: a
	// short-term or request-specific limit says little about the account,
	// so it must not bench it for long.
	briefCooldown = 30 * time.Second
	// maxCooldown is the longest Anthropic window (7 days) plus an hour of
	// clock skew; no limit legitimately lasts longer.
	maxCooldown = 7*24*time.Hour + time.Hour
	// resetJitter spreads a window reset by up to this much, so a pool's
	// clients do not all return to an account the instant it resets.
	resetJitter = 30 * time.Second
	// forbiddenCooldown benches an account that answered 403 to a request
	// another account then served.
	forbiddenCooldown = 30 * time.Minute
)

// limitScope is what a 429 limits.
type limitScope int

const (
	// limitAccount: the account (its subscription windows, or a short-term
	// limit); it cools down for every model.
	limitAccount limitScope = iota
	// limitModel: only the requested model on this account, a per-model
	// weekly limit or extra usage running out while the subscription
	// windows still have room. Other models keep using the account.
	limitModel
	// limitRequest: a refusal of this request on this account, not a limit
	// (a bare 429 without rate-limit headers). Other accounts are tried and
	// none is benched.
	limitRequest
	// limitFast: fast mode on an account without usage credits for it. It
	// goes straight back to the client, which falls back to normal speed;
	// walking the pool would only collect the same refusal everywhere.
	limitFast
)

// classify429 decides what a 429 limits and, for a limit, until when.
//
//   - a shared 5-hour or 7-day window rejected (or the subscription
//     rejected for no narrower reason): the account, until the latest
//     reset among the rejected windows, else exhaustedCooldown;
//   - fast mode without usage credits: the request (limitFast);
//   - no Retry-After or anthropic-ratelimit-* headers at all: the request
//     (limitRequest). OAuth tokens get that for requests Anthropic will
//     not serve on them, such as ones without Claude Code's system prompt;
//   - a per-model weekly limit (7d_oi) or extra usage rejected while the
//     shared windows have room: the model, until its reset;
//   - any other 429: the account, for Retry-After, else briefCooldown.
//
// Cooldowns are capped at maxCooldown.
func classify429(h http.Header, body []byte, now time.Time) (limitScope, time.Time) {
	unified := headerStatus(h, "Anthropic-Ratelimit-Unified-Status")
	switch {
	case sharedWindowRejected(h):
		return limitAccount, exhaustedUntil(h, now, true)
	case isFastCreditsRefusal(body):
		return limitFast, time.Time{}
	case !isRateLimit(h):
		return limitRequest, time.Time{}
	case modelOnlyRejection(h):
		return limitModel, exhaustedUntil(h, now, false)
	case unified == "rejected":
		return limitAccount, exhaustedUntil(h, now, true)
	}
	return limitAccount, retryAfter(h, now, briefCooldown)
}

// sharedWindowRejected reports whether the subscription's shared 5-hour or
// 7-day window is spent.
func sharedWindowRejected(h http.Header) bool {
	return headerStatus(h, "Anthropic-Ratelimit-Unified-5h-Status") == "rejected" ||
		headerStatus(h, "Anthropic-Ratelimit-Unified-7d-Status") == "rejected"
}

// modelOnlyRejection reports whether a rejection is of a per-model weekly
// limit (7d_oi) or of extra usage, while both shared windows still have
// room: the account can keep serving other models.
func modelOnlyRejection(h http.Header) bool {
	unified := headerStatus(h, "Anthropic-Ratelimit-Unified-Status")
	modelLimit := headerStatus(h, "Anthropic-Ratelimit-Unified-7d_oi-Status") == "rejected"
	claim := strings.ToLower(strings.TrimSpace(h.Get("Anthropic-Ratelimit-Unified-Representative-Claim")))
	overage := headerStatus(h, "Anthropic-Ratelimit-Unified-Overage-Status") == "rejected" ||
		strings.TrimSpace(h.Get("Anthropic-Ratelimit-Unified-Overage-Disabled-Reason")) != "" ||
		strings.Contains(claim, "overage")
	if !modelLimit && !(unified == "rejected" && overage) {
		return false
	}
	// The binding claim naming a shared window means the subscription is
	// spent, whatever else the headers say.
	if claim == "five_hour" || claim == "seven_day" {
		return false
	}
	return windowHasRoom(h, "5h") && windowHasRoom(h, "7d")
}

// windowHasRoom reports whether a shared window (5h, 7d) is not spent: its
// status allows requests, or, without a status, its utilization is below
// 100% or absent (Anthropic omits windows it did not evaluate).
func windowHasRoom(h http.Header, window string) bool {
	prefix := "Anthropic-Ratelimit-Unified-" + window + "-"
	switch headerStatus(h, prefix+"Status") {
	case "allowed", "allowed_warning":
		return true
	case "":
	default:
		return false
	}
	v := strings.TrimSpace(h.Get(prefix + "Utilization"))
	if v == "" {
		return true
	}
	u, err := strconv.ParseFloat(v, 64)
	return err == nil && u >= 0 && u < 1
}

// exhaustedUntil is when an exhausted limit resets: the latest reset among
// the rejected windows (shared ones for the account, 7d_oi for a model),
// plus the unified reset unless it is the extra-usage billing boundary,
// plus Retry-After for the account. Without any, exhaustedCooldown. A
// window reset gets up to resetJitter added.
func exhaustedUntil(h http.Header, now time.Time, account bool) time.Time {
	var until time.Time
	later := func(t time.Time) {
		if t.After(until) {
			until = t
		}
	}
	windows := []string{"7d_oi"}
	if account {
		windows = []string{"5h", "7d", "7d_oi"}
	}
	for _, w := range windows {
		prefix := "Anthropic-Ratelimit-Unified-" + w + "-"
		if headerStatus(h, prefix+"Status") == "rejected" {
			if t, ok := headerReset(h, prefix+"Reset"); ok {
				later(t)
			}
		}
	}
	if t, ok := headerReset(h, "Anthropic-Ratelimit-Unified-Reset"); ok && !isOverageBoundary(h, t) {
		later(t)
	}
	if !until.After(now) {
		until = time.Time{}
	} else {
		until = until.Add(rand.N(resetJitter) + time.Second)
	}
	if account && strings.TrimSpace(h.Get("Retry-After")) != "" {
		later(retryAfter(h, now, 0))
	}
	if !until.After(now) {
		return now.Add(exhaustedCooldown)
	}
	return capCooldown(until, now)
}

// isOverageBoundary reports whether a unified reset is the end of the extra
// usage billing period (the claim is overage and the two resets agree),
// which says nothing about when the limit lifts.
func isOverageBoundary(h http.Header, unified time.Time) bool {
	claim := strings.ToLower(h.Get("Anthropic-Ratelimit-Unified-Representative-Claim"))
	if !strings.Contains(claim, "overage") {
		return false
	}
	t, ok := headerReset(h, "Anthropic-Ratelimit-Unified-Overage-Reset")
	return ok && t.Equal(unified)
}

// isFastCreditsRefusal matches Anthropic's refusal of fast mode on an
// account without the usage credits for it ("Usage credits are required for
// fast mode"); a genuine rate limit never mentions fast mode.
func isFastCreditsRefusal(body []byte) bool {
	var e anthropicError
	msg := string(body)
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		msg = e.Error.Message
	}
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "fast request rejected") ||
		strings.Contains(msg, "fast") && (strings.Contains(msg, "usage credits") || strings.Contains(msg, "credits are required"))
}

// isRateLimit reports whether a 429 is an actual rate limit, i.e. carries
// Retry-After or anthropic-ratelimit-* headers.
func isRateLimit(h http.Header) bool {
	if strings.TrimSpace(h.Get("Retry-After")) != "" {
		return true
	}
	for k := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "Anthropic-Ratelimit-") {
			return true
		}
	}
	return false
}

// retryAfter parses Retry-After (delta-seconds or HTTP-date), falling back to
// def, capped at maxCooldown.
func retryAfter(h http.Header, now time.Time, def time.Duration) time.Time {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return now.Add(def)
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return capCooldown(now.Add(time.Duration(max(secs, 0))*time.Second), now)
	}
	if t, err := http.ParseTime(v); err == nil {
		if t.Before(now) {
			return now
		}
		return capCooldown(t, now)
	}
	return now.Add(def)
}

func capCooldown(t, now time.Time) time.Time {
	if limit := now.Add(maxCooldown); t.After(limit) {
		return limit
	}
	return t
}

func headerStatus(h http.Header, key string) string {
	return strings.ToLower(strings.TrimSpace(h.Get(key)))
}

// requestModel reads the model a Messages request asks for, or "". It stops
// at the top-level "model" key, which Claude Code sends first, instead of
// decoding the whole conversation.
func requestModel(body []byte) string {
	dec := json.NewDecoder(bytes.NewReader(body))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return ""
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return ""
		}
		if key == "model" {
			var m string
			if dec.Decode(&m) != nil {
				return ""
			}
			return m
		}
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return ""
		}
	}
	return ""
}
