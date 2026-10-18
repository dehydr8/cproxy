package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// UsageProbe reads an account's 5-hour and 7-day windows by having the
// account serve one minimal real Claude Code turn (claude-swap's "Haiku
// method"). It covers OAuth tokens the usage API refuses (no user:profile
// scope). The command layer supplies it; nil disables probing.
type UsageProbe func(ctx context.Context, token string) (*UsageAPI, error)

const (
	probeEvery   = 15 * time.Minute // per account, and only when headers are this stale
	probeTimeout = 90 * time.Second
)

// ParseRateLimitEvents reads the windows from the last rate_limit_event in a
// `claude -p --output-format stream-json --verbose` transcript. When there
// is none, the error carries the last api_retry HTTP status (how a revoked
// token shows up).
func ParseRateLimitEvents(transcript string, now time.Time) (*UsageAPI, error) {
	var out *UsageAPI
	status := 0
	sc := bufio.NewScanner(strings.NewReader(transcript))
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		var ev struct {
			Type        string `json:"type"`
			Subtype     string `json:"subtype"`
			ErrorStatus int    `json:"error_status"`
			Info        *struct {
				Windows map[string]*struct {
					Utilization *float64 `json:"utilization"`
					ResetsAt    *float64 `json:"resetsAt"`
				} `json:"unifiedWindows"`
			} `json:"rate_limit_info"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Subtype == "api_retry" && ev.ErrorStatus != 0 {
			status = ev.ErrorStatus
		}
		if ev.Type != "rate_limit_event" || ev.Info == nil {
			continue
		}
		u := &UsageAPI{FetchedAt: now, Source: "probe"}
		for key, dst := range map[string]**UsageAPIWindow{"five_hour": &u.FiveHour, "seven_day": &u.SevenDay} {
			w := ev.Info.Windows[key]
			if w == nil || w.Utilization == nil {
				continue
			}
			win := &UsageAPIWindow{Pct: math.Round(*w.Utilization*1000) / 10}
			if w.ResetsAt != nil {
				t := time.Unix(int64(*w.ResetsAt), 0)
				win.ResetsAt = &t
			}
			*dst = win
		}
		if u.FiveHour != nil || u.SevenDay != nil {
			out = u
		}
	}
	if out != nil {
		return out, nil
	}
	if status != 0 {
		return nil, fmt.Errorf("usage probe: no rate-limit event (HTTP %d)", status)
	}
	return nil, fmt.Errorf("usage probe: no rate-limit event")
}
