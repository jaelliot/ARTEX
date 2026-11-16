package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/internal/guard"
	"github.com/Autumn-27/artex/internal/worker"
	"github.com/Autumn-27/norma/agentcore"
	actool "github.com/Autumn-27/norma/tool"
)

func TestOfflineWorkerPolicyRemovesLocalAndInjectedTools(t *testing.T) {
	t.Setenv(worker.SocketEnv, "/unused.sock")
	opts := offlineWorkerOptions(agentcore.Options{
		Tools:          []actool.CoreTool{actool.NewBash(), actool.NewWebFetch(actool.WebFetchConfig{})},
		EnableWebFetch: true, EnableWebSearch: true, DeferredTools: []string{"dangerous"},
		Todos: actool.NewTodoStore(),
	})
	if len(opts.Tools) != 1 || opts.Tools[0].Name() != "Bash" {
		t.Fatalf("unexpected tools: %#v", opts.Tools)
	}
	if _, ok := opts.Tools[0].(*offlineBash); !ok {
		t.Fatal("Bash was not replaced with the offline worker")
	}
	if opts.EnableWebFetch || opts.EnableWebSearch || !opts.DisableBackgroundTasks || opts.Todos != nil || len(opts.DeferredTools) != 0 {
		t.Fatal("SDK-added tools remain enabled")
	}
	empty := offlineWorkerOptions(agentcore.Options{})
	if empty.Tools == nil || len(empty.Tools) != 0 {
		t.Fatal("empty tool list would fall back to SDK defaults")
	}
}

func TestOfflineWorkerPolicyLeavesUnconfiguredSessionsUnchanged(t *testing.T) {
	t.Setenv(worker.SocketEnv, "")
	if err := os.Unsetenv(worker.SocketEnv); err != nil {
		t.Fatal(err)
	}
	tool := actool.NewBash()
	opts := offlineWorkerOptions(agentcore.Options{Tools: []actool.CoreTool{tool}, EnableWebSearch: true})
	if len(opts.Tools) != 1 || opts.Tools[0] != tool || !opts.EnableWebSearch {
		t.Fatal("unconfigured session was modified")
	}
}

func TestOfflineChatRoutesModelBashToBroker(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "must-not-run-locally")
	path := filepath.Join(dir, "broker.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv(worker.SocketEnv, path)
	requests := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadBytes('\n')
		requests <- string(line)
		_, _ = conn.Write([]byte(`{"ok":true,"output":"offline fixture"}` + "\n"))
	}()
	input, _ := json.Marshal(map[string]string{"command": "touch " + marker})
	chat := NewChatAgent(toolCallThenStop("Bash", string(input)), "test-model", dir, nil, 0)
	chat.SetGuard(guard.New())
	if _, err := chat.Chat(context.Background(), "assistant", "offline-fixture", "run a test", 3, 0, false, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-requests:
		var request map[string]string
		if err := json.Unmarshal([]byte(line), &request); err != nil || request["script"] != "touch "+marker {
			t.Fatalf("unexpected broker request: %s (%v)", line, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("model tool call did not reach the broker")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("model command ran in the server: %v", err)
	}
}

func TestOfflineChatRejectsModelWebFetch(t *testing.T) {
	var hits atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer fake.Close()
	t.Setenv(worker.SocketEnv, "/unused.sock")
	input, _ := json.Marshal(map[string]string{"url": fake.URL})
	chat := NewChatAgent(toolCallThenStop("WebFetch", string(input)), "test-model", t.TempDir(), nil, 0)
	chat.SetGuard(guard.New())
	_, _ = chat.Chat(context.Background(), "assistant", "offline-fetch", "run a test", 3, 0, false, nil)
	if got := hits.Load(); got != 0 {
		t.Fatalf("offline mode made %d WebFetch requests", got)
	}
}

func TestOfflineChatThroughBrokerAndWorkerLauncher(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	dir := t.TempDir()
	sandboxDir, err := filepath.Abs("../../sandbox")
	if err != nil {
		t.Fatal(err)
	}
	fakeDocker := filepath.Join(dir, "docker")
	dockerScript := `#!/bin/sh
case "$1 $2" in
  "image inspect") if [ "$4" = '{{.Id}}' ]; then echo sha256:fixture; else echo 0; fi ;;
  "inspect "*) exit 1 ;;
  "create "*) printf '%s\n' "$@" > "$FAKE_DIR/create.args" ;;
  "start "*) cat > "$FAKE_DIR/received"; echo worker-fixture ;;
  "rm -f") touch "$FAKE_DIR/removed" ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(fakeDocker, []byte(dockerScript), 0o700); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(dir, "launcher")
	launcherScript := fmt.Sprintf("#!/bin/sh\nexport FAKE_DIR=%q\nexport PATH=%q:/usr/bin:/bin\nexec %q\n", dir, dir, filepath.Join(sandboxDir, "offline-worker"))
	if err := os.WriteFile(launcher, []byte(launcherScript), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "broker.sock")
	code := `import pathlib,socketserver,sys
sys.path.insert(0,sys.argv[2])
import worker_broker as broker
original=broker.run_worker
broker.run_worker=lambda script,canceled=None: original(script,pathlib.Path(sys.argv[3]),canceled)
server=socketserver.UnixStreamServer(sys.argv[1],broker.WorkerHandler)
server.handle_request()
server.server_close()
`
	cmd := exec.Command(python, "-B", "-c", code, path, sandboxDir, launcher)
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("broker did not start: %v", err)
	}
	t.Setenv(worker.SocketEnv, path)
	marker := filepath.Join(dir, "must-not-run")
	input, _ := json.Marshal(map[string]string{"command": "touch " + marker})
	chat := NewChatAgent(toolCallThenStop("Bash", string(input)), "test-model", dir, nil, 0)
	chat.SetGuard(guard.New())
	if _, err := chat.Chat(context.Background(), "assistant", "offline-e2e", "run a test", 3, 0, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		finished = true
		t.Fatalf("broker failed: %v: %s", err, log.String())
	}
	finished = true
	command, err := os.ReadFile(filepath.Join(dir, "received"))
	if err != nil || string(command) != "touch "+marker {
		t.Fatalf("unexpected worker input %q: %v", command, err)
	}
	args, err := os.ReadFile(filepath.Join(dir, "create.args"))
	if err != nil || !strings.Contains(string(args), "--read-only\n") || !strings.Contains(string(args), "--network\nnone\n") {
		t.Fatalf("worker restrictions missing: %q: %v", args, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "removed")); err != nil {
		t.Fatalf("worker was not cleaned up: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("command executed in the server: %v", err)
	}
}

func TestOfflineWorkerSkipsToolAugmentation(t *testing.T) {
	t.Setenv(worker.SocketEnv, "/unused.sock")
	previousAugment, previousResolve := ToolAugment(), ToolResolve()
	t.Cleanup(func() { SetToolAugment(previousAugment); SetToolResolve(previousResolve) })
	called := false
	SetToolResolve(nil)
	SetToolAugment(func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
		called = true
		return nil, DeferredInfo{}, nil
	})
	_, _, cleanup := AugmentTools(context.Background(), "worker", []actool.CoreTool{actool.NewBash()})
	cleanup()
	if called {
		t.Fatal("tool augmentation ran in offline worker mode")
	}
}

func TestOfflineBashUsesBrokerAndDoesNotFallBack(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "unexpected")
	missing := &offlineBash{CoreTool: actool.NewBash(), socket: filepath.Join(dir, "missing.sock")}
	input, _ := json.Marshal(map[string]string{"command": "touch " + marker})
	result, err := missing.Call(context.Background(), input, nil)
	if err != nil || !result.IsError {
		t.Fatalf("missing broker did not fail closed: %+v %v", result, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("command ran locally: %v", err)
	}

	path := filepath.Join(dir, "worker.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requests := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadBytes('\n')
		requests <- string(line)
		_, _ = conn.Write([]byte(`{"ok":true,"output":"worker-result"}` + "\n"))
	}()
	b := &offlineBash{CoreTool: actool.NewBash(), socket: path}
	result, err = b.Call(context.Background(), json.RawMessage(`{"command":"echo hello"}`), nil)
	if err != nil || result.IsError || result.Flatten() != "worker-result" {
		t.Fatalf("worker result: %+v %v", result, err)
	}
	var request map[string]string
	if err := json.Unmarshal([]byte(<-requests), &request); err != nil || len(request) != 1 || request["script"] != "echo hello" {
		t.Fatalf("unexpected broker request: %#v %v", request, err)
	}
	result, err = b.Call(context.Background(), json.RawMessage(`{"command":"true","run_in_background":true}`), nil)
	if err != nil || !result.IsError {
		t.Fatal("background command was accepted")
	}
}
