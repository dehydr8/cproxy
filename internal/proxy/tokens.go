package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/dehydr8/cproxy/internal/pool"
)

// Token accounting: a copy of each successful /v1/messages response is
// scanned for the usage Anthropic reports — SSE message_start and
// message_delta events, or the usage object of a plain JSON body — while
// the response streams to the client unchanged.

const (
	maxUsageLine = 1 << 20 // longest SSE line scanned; longer ones are skipped
	maxUsageBody = 4 << 20 // largest plain JSON body scanned
)

type apiUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// usageScanner is an io.Writer that extracts token usage from what is
// written to it.
type usageScanner struct {
	sse  bool
	line []byte
	skip bool // inside an over-long SSE line
	body bytes.Buffer
	u    pool.TokenUsage
}

func (sc *usageScanner) Write(p []byte) (int, error) {
	n := len(p)
	if !sc.sse {
		if sc.body.Len()+len(p) <= maxUsageBody {
			sc.body.Write(p)
		}
		return n, nil
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			sc.appendLine(p)
			return n, nil
		}
		sc.appendLine(p[:i])
		if !sc.skip {
			sc.event(sc.line)
		}
		sc.line, sc.skip, p = sc.line[:0], false, p[i+1:]
	}
	return n, nil
}

func (sc *usageScanner) appendLine(p []byte) {
	if sc.skip || len(sc.line)+len(p) > maxUsageLine {
		sc.skip, sc.line = true, sc.line[:0]
		return
	}
	sc.line = append(sc.line, p...)
}

// event handles one SSE line; usage counts are cumulative, so keep maxima.
func (sc *usageScanner) event(line []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r"), []byte("data:"))
	if !ok {
		return
	}
	var ev struct {
		Message *struct {
			Usage *apiUsage `json:"usage"`
		} `json:"message"`
		Usage *apiUsage `json:"usage"`
	}
	if json.Unmarshal(bytes.TrimSpace(data), &ev) != nil {
		return
	}
	if ev.Message != nil && ev.Message.Usage != nil {
		sc.merge(*ev.Message.Usage)
	}
	if ev.Usage != nil {
		sc.merge(*ev.Usage)
	}
}

func (sc *usageScanner) merge(u apiUsage) {
	sc.u.Input = max(sc.u.Input, u.InputTokens)
	sc.u.Output = max(sc.u.Output, u.OutputTokens)
	sc.u.CacheWrite = max(sc.u.CacheWrite, u.CacheCreationInputTokens)
	sc.u.CacheRead = max(sc.u.CacheRead, u.CacheReadInputTokens)
}

// result returns the usage seen (for SSE, as far as the stream got).
func (sc *usageScanner) result() pool.TokenUsage {
	if !sc.sse {
		var body struct {
			Usage *apiUsage `json:"usage"`
		}
		if json.Unmarshal(sc.body.Bytes(), &body) == nil && body.Usage != nil {
			sc.merge(*body.Usage)
		}
	}
	return sc.u
}

// tapUsage returns a reader that passes resp.Body through unchanged while
// scanning a copy for token usage, and a finish func that reports it.
// Unscannable encodings (anything but identity and gzip) are passed through
// without counting.
func tapUsage(resp *http.Response, report func(pool.TokenUsage)) (io.Reader, func()) {
	sc := &usageScanner{sse: strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")}
	finish := func() {
		if u := sc.result(); !u.IsZero() {
			report(u)
		}
	}
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "", "identity":
		return io.TeeReader(resp.Body, sc), finish
	case "gzip":
		pr, pw := io.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			if zr, err := gzip.NewReader(pr); err == nil {
				io.Copy(sc, zr)
			}
			io.Copy(io.Discard, pr) // never block the client stream
		}()
		return io.TeeReader(resp.Body, pw), func() {
			pw.Close()
			<-done
			finish()
		}
	default:
		return resp.Body, func() {}
	}
}
