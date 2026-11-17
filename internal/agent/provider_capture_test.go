package agent

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/internal/llmrec"
)

func TestRequestSnapshotDoesNotLimitLargePrompt(t *testing.T) {
	prompt := strings.Repeat("x", llmrec.MaxCaptureBytes+1024)
	for _, recording := range []bool{false, true} {
		req, _ := http.NewRequest(http.MethodPost, "http://model.invalid", strings.NewReader(prompt))
		copies := 0
		getBody := req.GetBody
		req.GetBody = func() (io.ReadCloser, error) { copies++; return getBody() }
		var capt *llmrec.Capture
		if recording {
			ctx, c := llmrec.NewCapture(req.Context())
			req, capt = req.WithContext(ctx), c
		}
		transport := quotaAwareTransport{base: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			n, err := io.Copy(io.Discard, r.Body)
			if err != nil || n != int64(len(prompt)) {
				t.Fatalf("prompt changed on wire: n=%d err=%v", n, err)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
		})}
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if !recording && copies != 0 {
			t.Fatal("copied request with recording disabled")
		}
		if recording && (copies != 1 || len(capt.RawRequest()) > llmrec.MaxCaptureBytes+128 || !strings.Contains(capt.RawRequest(), "[recording truncated]")) {
			t.Fatal("large request recording was not bounded and marked")
		}
	}
}

// The transport is the only layer that still sees the wire bodies: norma builds
// the request body internally and decodes the SSE response before the recorder
// gets it. This checks the round trip preserves both directions untouched.
func TestRoundTripCapturesRawBodies(t *testing.T) {
	const sse = "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer srv.Close()

	client, err := quotaAwareHTTPClient("", "", "")
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	const reqBody = `{"model":"claude","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"t","input_schema":{}}]}`
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	ctx, capt := llmrec.NewCapture(req.Context())
	req = req.WithContext(ctx)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = resp.Body.Close()

	if string(got) != sse {
		t.Fatal("capture altered the response delivered to norma")
	}
	if capt.RawRequest() != reqBody {
		t.Fatalf("RawRequest()=%q want %q", capt.RawRequest(), reqBody)
	}
	if capt.RawResponse() != sse {
		t.Fatalf("RawResponse()=%q want the raw SSE frames", capt.RawResponse())
	}
}

// A 429 body is read and replaced in-place by the quota check. Capturing must
// still see it, and the replacement body must remain readable downstream.
func TestRoundTripCaptures429BodyAlongsideQuotaRewrite(t *testing.T) {
	const body = `{"error":{"message":"insufficient_quota"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	client, err := quotaAwareHTTPClient("", "", "")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	ctx, capt := llmrec.NewCapture(req.Context())
	req = req.WithContext(ctx)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	// Quota exhaustion is normalized to 402 so the router fails over.
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status=%d want 402", resp.StatusCode)
	}
	if capt.RawResponse() != body {
		t.Fatalf("RawResponse()=%q want %q", capt.RawResponse(), body)
	}
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read replaced body: %v", err)
	}
	if string(rest) != body {
		t.Fatalf("replaced body=%q want it still readable", rest)
	}
}

// Recording off = no Capture on the context. The transport must behave exactly
// as before, including the quota rewrite.
func TestRoundTripWithoutCaptureIsUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	client, err := quotaAwareHTTPClient("", "", "")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	resp, err := client.Post(srv.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ok" {
		t.Fatalf("body=%q", got)
	}
}
