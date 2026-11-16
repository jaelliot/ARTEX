package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testListener(t *testing.T) (net.Listener, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener, path
}

func TestRunSendsOnlyScript(t *testing.T) {
	listener, path := testListener(t)
	received := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadBytes('\n')
		received <- string(line)
		_, _ = conn.Write([]byte(`{"ok":true,"output":"done"}` + "\n"))
	}()
	result, err := Run(context.Background(), path, "printf 'hello'\n")
	if err != nil || !result.OK || result.Output != "done" {
		t.Fatalf("Run = %+v, %v", result, err)
	}
	var request map[string]string
	if err := json.Unmarshal([]byte(<-received), &request); err != nil || len(request) != 1 || request["script"] != "printf 'hello'\n" {
		t.Fatalf("unexpected request: %#v, %v", request, err)
	}
}

func TestRunCancelClosesConnection(t *testing.T) {
	listener, path := testListener(t)
	connected := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadBytes('\n')
		close(connected)
		var b [1]byte
		_, _ = conn.Read(b[:])
		close(closed)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := Run(ctx, path, "sleep 20"); done <- err }()
	<-connected
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want cancellation", err)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("client connection remained open after cancellation")
	}
}

func TestRunRejectsInvalidRequests(t *testing.T) {
	for _, test := range []struct{ path, script string }{
		{"relative.sock", "echo hello"},
		{"/unused.sock", strings.Repeat("x", maxRequest)},
	} {
		if _, err := Run(context.Background(), test.path, test.script); err == nil {
			t.Fatalf("request unexpectedly accepted: path=%q, script length=%d", test.path, len(test.script))
		}
	}
}

func TestRunRejectsUnknownResponseFields(t *testing.T) {
	listener, path := testListener(t)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadBytes('\n')
		_, _ = conn.Write([]byte(`{"ok":true,"extra":"unexpected"}` + "\n"))
	}()
	if _, err := Run(context.Background(), path, "true"); err == nil {
		t.Fatal("unexpected response was accepted")
	}
}
