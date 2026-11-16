package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Autumn-27/artex/internal/worker"
)

func TestOfflineWorkerModeBlocksDirectCustomToolsAndMCP(t *testing.T) {
	t.Setenv(worker.SocketEnv, "") // configured but invalid is still fail-closed
	s := &Server{}
	raw := json.RawMessage(`{"command":"true","code":"print(1)","url":"http://127.0.0.1"}`)
	for name, call := range map[string]func() bool{
		"command": func() bool {
			result, _ := s.runCommandTool(context.Background(), "test", raw, nil, nil)
			return result.IsError
		},
		"script": func() bool {
			result, _ := s.runScriptTool(context.Background(), "test", raw, nil, nil)
			return result.IsError
		},
		"http": func() bool {
			result, _ := s.runHTTPTool(context.Background(), "test", raw, nil, nil)
			return result.IsError
		},
	} {
		if !call() {
			t.Errorf("%s custom tool was not blocked", name)
		}
	}
	if _, err := connectMCP(context.Background(), nil); err == nil {
		t.Fatal("MCP connection was not blocked")
	}
	if _, err := s.scopeSentryClient(context.Background()); err == nil {
		t.Fatal("ScopeSentry connection was not blocked")
	}
	recorder := httptest.NewRecorder()
	s.notifyTestChannel(recorder, httptest.NewRequest(http.MethodPost, "/", nil))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("notification test returned %d", recorder.Code)
	}
	called := false
	updates := &updateCheck{enabled: true, fetch: func(context.Context) (string, error) {
		called = true
		return "v99.0.0", nil
	}}
	if got := updates.newerVersion(context.Background(), time.Now(), "v1.0.0"); got != "" || called {
		t.Fatal("update check ran in offline worker mode")
	}
}
