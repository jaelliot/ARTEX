package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRenderTemplateCommand(t *testing.T) {
	// command: string params shell-quoted; array → JSON (also quoted).
	got := renderTemplate("nmap -p {ports} {target}", map[string]any{
		"target": "10.0.0.1; rm -rf /", // injection attempt → must be single-quoted
		"ports":  []any{80.0, 443.0},
	}, shellQuote)
	if !strings.Contains(got, `'10.0.0.1; rm -rf /'`) {
		t.Fatalf("target not shell-quoted: %q", got)
	}
	if strings.Contains(got, "rm -rf /'") && !strings.Contains(got, `'10.0.0.1; rm -rf /'`) {
		t.Fatalf("possible injection leak: %q", got)
	}
	if strings.Contains(got, "{target}") || strings.Contains(got, "{ports}") {
		t.Fatalf("placeholders not replaced: %q", got)
	}
}

func TestRenderTemplateHTTP(t *testing.T) {
	// http: identity (no shell quoting) — raw substitution into url/body.
	got := renderTemplate("https://x/submit?flag={flag}", map[string]any{"flag": "CTF{abc}"}, identity)
	if got != "https://x/submit?flag=CTF{abc}" {
		t.Fatalf("http render: %q", got)
	}
}

func TestScalarStr(t *testing.T) {
	cases := []struct {
		v    any
		want string
		ok   bool
	}{
		{"hi", "hi", true},
		{true, "true", true},
		{float64(80), "80", true},   // integer-valued float → no decimal
		{float64(1.5), "1.5", true}, // real float
		{[]any{1, 2}, "", false},    // array → not scalar
		{map[string]any{}, "", false},
	}
	for _, c := range cases {
		got, ok := scalarStr(c.v)
		if ok != c.ok || (ok && got != c.want) {
			t.Fatalf("scalarStr(%v) = (%q,%v), want (%q,%v)", c.v, got, ok, c.want, c.ok)
		}
	}
}

func TestEnsureSchema(t *testing.T) {
	// empty → thin {args:string}
	m := ensureSchema(nil)
	props, _ := m["properties"].(map[string]any)
	if _, ok := props["args"]; !ok {
		t.Fatalf("empty schema should default to args: %v", m)
	}
	// non-empty → passthrough
	raw := json.RawMessage(`{"type":"object","properties":{"target":{"type":"string"}}}`)
	m2 := ensureSchema(raw)
	p2, _ := m2["properties"].(map[string]any)
	if _, ok := p2["target"]; !ok {
		t.Fatalf("non-empty schema should pass through: %v", m2)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("a'b"); got != `'a'\''b'` {
		t.Fatalf("shellQuote(a'b) = %q", got)
	}
}

// TestExecPython runs a real Python script end-to-end: it must read params from
// stdin JSON and the mirrored env var, then print — verifying the whole script
// param-passing path. Skips if no python3.
func TestExecPython(t *testing.T) {
	interp, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	code := `import json,sys,os
a = json.load(sys.stdin)
print("stdin_target=" + a["target"])
print("env_target=" + os.environ.get("TOOL_TARGET",""))
print("port=" + str(a["port"]))
`
	out, err := execPython(context.Background(), interp, "test", code,
		map[string]any{"target": "example.com", "port": float64(8080)},
		t.TempDir(), nil, 10*time.Second)
	if err != nil {
		t.Fatalf("execPython error: %v", err)
	}
	for _, want := range []string{"stdin_target=example.com", "env_target=example.com", "port=8080"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestExecPythonBoundsOutput(t *testing.T) {
	interp, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	out, err := execPython(context.Background(), interp, "test", `print("x" * 2000000)`, nil, t.TempDir(), nil, 5*time.Second)
	if err != nil || !strings.Contains(out, "[script output truncated]") || len(out) > maxScriptOutput+128 {
		t.Fatalf("output was not bounded: len=%d err=%v", len(out), err)
	}
}

func TestExecPythonTimeoutStopsChild(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process-group cleanup is implemented on Linux")
	}
	interp, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	dir := t.TempDir()
	code := `import subprocess,sys,time
subprocess.Popen([sys.executable, "-c", "import time; open('child-started', 'w').close(); time.sleep(1); open('child-ran', 'w').close()"])
time.sleep(10)`
	out, err := execPython(context.Background(), interp, "test", code, nil, dir, nil, 500*time.Millisecond)
	if err != nil || !strings.Contains(out, "[terminated on timeout]") {
		t.Fatalf("script did not time out: %q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "child-started")); err != nil {
		t.Fatalf("child did not start before cancellation: %v", err)
	}
	time.Sleep(time.Second)
	if _, err := os.Stat(filepath.Join(dir, "child-ran")); !os.IsNotExist(err) {
		t.Fatalf("child continued after cancellation: %v", err)
	}
}

func TestCustomToolTimeoutBound(t *testing.T) {
	if got := timeoutOr(int(^uint(0)>>1), 1000); got != 10*time.Minute {
		t.Fatalf("timeout = %v, want 10m", got)
	}
}

func TestExecPythonParentExitStopsChild(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process-group cleanup is implemented on Linux")
	}
	interp, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	for _, redirect := range []bool{false, true} {
		name := "inherited-pipes"
		if redirect {
			name = "closed-pipes"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			code := `import os,subprocess,sys,time,pathlib
kwargs = {}
if os.environ.get("TOOL_REDIRECT") == "true":
    kwargs = dict(stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
subprocess.Popen([sys.executable, "-c", "import time; open('child-started', 'w').close(); time.sleep(3); open('child-ran', 'w').close()"], **kwargs)
deadline = time.monotonic() + 2
while not pathlib.Path('child-started').exists() and time.monotonic() < deadline:
    time.sleep(0.01)
`
			out, err := execPython(context.Background(), interp, "test", code,
				map[string]any{"redirect": redirect}, dir, nil, 10*time.Second)
			if err != nil || strings.Contains(out, "[exit:") {
				t.Fatalf("script failed: %q %v", out, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "child-started")); err != nil {
				t.Fatalf("child did not start before parent exited: %v", err)
			}
			time.Sleep(3250 * time.Millisecond)
			if _, err := os.Stat(filepath.Join(dir, "child-ran")); !os.IsNotExist(err) {
				t.Fatalf("child survived parent exit: %v", err)
			}
		})
	}
}

// TestRunHTTPTool functionally exercises the http executor against a local server:
// method/url/header/body templates are rendered from params, the request is sent,
// and the response status+body are returned. No recording proxy → s.m untouched.
func TestRunHTTPTool(t *testing.T) {
	var gotMethod, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	execRaw, _ := json.Marshal(map[string]any{
		"method":  "POST",
		"url":     srv.URL + "/submit?flag={flag}",
		"headers": map[string]string{"Authorization": "Bearer {token}"},
		"body":    `{"flag":"{flag}"}`,
	})
	res, err := (&Server{}).runHTTPTool(context.Background(), "probe", execRaw,
		map[string]any{"flag": "CTF{x}", "token": "sekret"}, nil)
	if err != nil {
		t.Fatalf("runHTTPTool: %v", err)
	}
	if res.IsError || len(res.Content) == 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	var out struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	_ = json.Unmarshal([]byte(res.Content[0].Text), &out)

	if gotMethod != "POST" {
		t.Fatalf("method: %q", gotMethod)
	}
	if gotAuth != "Bearer sekret" {
		t.Fatalf("header template not rendered: %q", gotAuth)
	}
	if gotBody != `{"flag":"CTF{x}"}` {
		t.Fatalf("body template not rendered: %q", gotBody)
	}
	if out.Status != 201 || !strings.Contains(out.Body, `"ok":true`) {
		t.Fatalf("response not captured: %+v", out)
	}
}

func TestDetectPython(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	if p := detectPython(); p == "" {
		t.Fatal("detectPython returned empty despite python3 on PATH")
	}
}

// The http tool must truncate an oversized response through norma's Capture, the
// same valve every other tool uses — a session with no OutputDir falls back to a
// head+tail cut, so a body far past the default cap comes back shortened.
func TestHTTPToolTruncatesLargeBody(t *testing.T) {
	big := strings.Repeat("x", 40000) // past Capture's 30000 default
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	execRaw, _ := json.Marshal(map[string]any{"method": "GET", "url": srv.URL})
	res, err := (&Server{}).runHTTPTool(context.Background(), "probe", execRaw, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("runHTTPTool: %v", err)
	}
	out := res.Flatten()
	if len(out) >= 40000 {
		t.Fatalf("oversized http body was not truncated: len=%d", len(out))
	}
}
