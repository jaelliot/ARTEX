package llmrec

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestCaptureFromNilAndMissing(t *testing.T) {
	var nilCtx context.Context // the transport reaches here on any unrecorded call
	if CaptureFrom(nilCtx) != nil {
		t.Fatal("nil context should yield no capture")
	}
	if CaptureFrom(context.Background()) != nil {
		t.Fatal("bare context should yield no capture")
	}
}

// A nil *Capture is the "recording off" path: the transport calls these on every
// request, so they must all be no-ops rather than panics.
func TestNilCaptureIsInert(t *testing.T) {
	var c *Capture
	c.SetRequest("body")
	if got := c.RawRequest(); got != "" {
		t.Fatalf("RawRequest()=%q want empty", got)
	}
	if got := c.RawResponse(); got != "" {
		t.Fatalf("RawResponse()=%q want empty", got)
	}
	rc := io.NopCloser(strings.NewReader("x"))
	if c.TeeResponse(200, rc) != rc {
		t.Fatal("nil capture must pass the body through untouched")
	}
}

func TestCaptureSingleAttemptKeepsBytesVerbatim(t *testing.T) {
	ctx, c := NewCapture(context.Background())
	if CaptureFrom(ctx) != c {
		t.Fatal("capture not retrievable from its own context")
	}
	c.SetRequest(`{"model":"x"}`)
	// Retries re-send identical bytes; only the first is kept.
	c.SetRequest(`{"model":"ignored"}`)

	const sse = "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {}\n\n"
	body := c.TeeResponse(200, io.NopCloser(strings.NewReader(sse)))
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != sse {
		t.Fatal("tee altered the stream delivered to the caller")
	}
	if err := body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if c.RawRequest() != `{"model":"x"}` {
		t.Fatalf("RawRequest()=%q", c.RawRequest())
	}
	// A lone attempt must stay byte-identical — no headers, no framing added, so
	// it can be replayed as-is.
	if c.RawResponse() != sse {
		t.Fatalf("RawResponse()=%q want verbatim SSE", c.RawResponse())
	}
}

// norma's doStream discards the bodies of retried attempts (retry.go closes them
// unread), so the capture is the only place a 429 body survives.
func TestCaptureRetriedAttemptsAreAllKept(t *testing.T) {
	_, c := NewCapture(context.Background())

	first := c.TeeResponse(429, io.NopCloser(strings.NewReader(`{"error":"rate_limit"}`)))
	if _, err := io.ReadAll(first); err != nil {
		t.Fatalf("read first: %v", err)
	}
	second := c.TeeResponse(200, io.NopCloser(strings.NewReader("data: ok\n")))
	if _, err := io.ReadAll(second); err != nil {
		t.Fatalf("read second: %v", err)
	}

	raw := c.RawResponse()
	if !strings.Contains(raw, `{"error":"rate_limit"}`) {
		t.Fatalf("dropped the retried attempt body: %q", raw)
	}
	if !strings.Contains(raw, "data: ok") {
		t.Fatalf("dropped the final attempt body: %q", raw)
	}
	if !strings.Contains(raw, "attempt 1/2 — HTTP 429") || !strings.Contains(raw, "attempt 2/2 — HTTP 200") {
		t.Fatalf("attempts not delimited: %q", raw)
	}
	if strings.Index(raw, "rate_limit") > strings.Index(raw, "data: ok") {
		t.Fatal("attempts stored out of order")
	}
}

func TestCaptureEmptyWhenNoHTTPHappened(t *testing.T) {
	_, c := NewCapture(context.Background())
	if c.RawRequest() != "" || c.RawResponse() != "" {
		t.Fatal("unused capture should be empty")
	}
}

func TestCaptureBoundsRetriesWithoutChangingStreams(t *testing.T) {
	_, c := NewCapture(context.Background())
	chunk := strings.Repeat("x", MaxCaptureBytes/2+1024)
	for i := 0; i < maxCaptureAttempts+2; i++ {
		body := c.TeeResponse(200, io.NopCloser(strings.NewReader(chunk)))
		n, err := io.Copy(io.Discard, body)
		body.Close()
		if err != nil || n != int64(len(chunk)) {
			t.Fatalf("stream truncated: n=%d err=%v", n, err)
		}
	}
	attempts := c.Attempts()
	if len(attempts) != maxCaptureAttempts || c.responseBytes != MaxCaptureBytes {
		t.Fatal("retry capture exceeds its budget")
	}
	if !strings.Contains(attempts[1].Body, "[recording truncated]") || !strings.Contains(c.RawResponse(), "[additional recording attempts omitted]") {
		t.Fatal("recording truncation is not visible")
	}
	if len(c.RawResponse()) > MaxCaptureBytes+4096 {
		t.Fatal("recorded response grew beyond bounded metadata")
	}
	c.SetRequest(strings.Repeat("q", MaxCaptureBytes+1024))
	if len(c.RawRequest()) > MaxCaptureBytes+128 || !strings.Contains(c.RawRequest(), "[recording truncated]") {
		t.Fatal("request recording exceeds budget")
	}
}

func TestCaptureSnapshotsWhileReading(t *testing.T) {
	_, c := NewCapture(context.Background())
	body := c.TeeResponse(200, io.NopCloser(strings.NewReader(strings.Repeat("x", MaxCaptureBytes+1024))))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, body)
		_ = body.Close()
	}()
	for i := 0; i < 10; i++ {
		_ = c.RawResponse()
		_ = c.Attempts()
	}
	<-done
	if !strings.Contains(c.RawResponse(), "[recording truncated]") {
		t.Fatal("concurrent recording lost its truncation marker")
	}
}
