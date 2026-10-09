package provider

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// HTTP is the shared upstream HTTP client.
type HTTP struct {
	Client *http.Client
}

// NewHTTP returns a client tuned for long-lived streaming connections.
func NewHTTP() *HTTP {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &HTTP{Client: &http.Client{Transport: tr}}
}

const maxErrorBody = 1 << 20
const maxResponseBody = 64 << 20

// do performs a JSON POST. For non-streaming calls the whole exchange is bounded
// by timeout. For streaming calls timeout bounds time-to-headers and the gap
// between chunks (idle timeout); the returned watchdog must be poked per chunk.
func (h *HTTP) do(ctx context.Context, provider, url string, headers map[string]string, body []byte, timeout time.Duration, stream bool) (*http.Response, *watchdog, error) {
	ctx, cancel := context.WithCancel(ctx)
	wd := newWatchdog(timeout, cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		wd.stop()
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		timedOut := wd.fired()
		wd.stop()
		msg := err.Error()
		if timedOut {
			msg = fmt.Sprintf("timeout after %s", timeout)
		}
		var ue = &UpstreamError{Provider: provider, Message: msg, Err: err}
		if !timedOut && errors.Is(err, context.Canceled) {
			return nil, nil, err // caller canceled: not an upstream failure
		}
		return nil, nil, ue
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		resp.Body.Close()
		wd.stop()
		return nil, nil, errorFromResponse(provider, resp, b)
	}
	if stream {
		wd.poke()
	}
	return resp, wd, nil
}

// readAll reads a non-streaming body under the watchdog.
func readAll(provider string, resp *http.Response, wd *watchdog) ([]byte, error) {
	defer wd.stop()
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		msg := err.Error()
		if wd.fired() {
			msg = "timeout reading response"
		}
		return nil, &UpstreamError{Provider: provider, Message: msg, Err: err}
	}
	return b, nil
}

// watchdog cancels a request when no progress happens within d.
type watchdog struct {
	mu     sync.Mutex
	d      time.Duration
	t      *time.Timer
	cancel context.CancelFunc
	fire   bool
}

func newWatchdog(d time.Duration, cancel context.CancelFunc) *watchdog {
	w := &watchdog{d: d, cancel: cancel}
	w.t = time.AfterFunc(d, func() {
		w.mu.Lock()
		w.fire = true
		w.mu.Unlock()
		cancel()
	})
	return w
}

func (w *watchdog) poke() { w.t.Reset(w.d) }
func (w *watchdog) fired() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fire
}
func (w *watchdog) stop() {
	w.t.Stop()
	w.cancel()
}

// sseEvent is one server-sent event.
type sseEvent struct {
	Event string
	Data  []byte
}

// sseReader parses a text/event-stream body.
type sseReader struct {
	r *bufio.Reader
}

func newSSEReader(r io.Reader) *sseReader { return &sseReader{r: bufio.NewReaderSize(r, 64<<10)} }

const maxSSELine = 16 << 20

// Next returns the next event with a non-empty data payload.
func (s *sseReader) Next() (sseEvent, error) {
	var ev sseEvent
	var data bytes.Buffer
	hasData := false
	for {
		line, err := s.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) && hasData {
				ev.Data = data.Bytes()
				return ev, nil
			}
			return ev, err
		}
		if len(line) == 0 {
			if hasData {
				ev.Data = data.Bytes()
				return ev, nil
			}
			ev.Event = ""
			continue
		}
		if line[0] == ':' {
			continue // comment / keep-alive
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			ev.Event = string(value)
		case "data":
			if hasData {
				data.WriteByte('\n')
			}
			data.Write(value)
			hasData = true
		}
	}
}

func (s *sseReader) readLine() ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := s.r.ReadLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk...)
		if len(buf) > maxSSELine {
			return nil, errors.New("SSE line too long")
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

// streamBase holds the plumbing shared by all stream implementations.
type streamBase struct {
	provider string
	body     io.ReadCloser
	wd       *watchdog
	sse      *sseReader
	usage    Usage
	hasUsage bool
}

func newStreamBase(provider string, resp *http.Response, wd *watchdog) streamBase {
	return streamBase{provider: provider, body: resp.Body, wd: wd, sse: newSSEReader(resp.Body)}
}

func (b *streamBase) next() (sseEvent, error) {
	ev, err := b.sse.Next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return ev, io.EOF
		}
		msg := err.Error()
		if b.wd.fired() {
			msg = "stream idle timeout"
		}
		return ev, &UpstreamError{Provider: b.provider, Message: msg, Err: err}
	}
	b.wd.poke()
	return ev, nil
}

func (b *streamBase) Usage() (Usage, bool) { return b.usage, b.hasUsage }

func (b *streamBase) Close() error {
	b.wd.stop()
	return b.body.Close()
}
