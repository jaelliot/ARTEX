package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/internal/agent"
	"github.com/Autumn-27/artex/internal/db"
	"github.com/Autumn-27/norma/llm"
)

func TestKeylessOpenAIProviderDoesNotSendCredentials(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "cloud-secret")
	calls := 0
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "" {
			t.Errorf("path=%q authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer fake.Close()
	cfg := agent.ConfigFrom("openai-keyless", "test-model", fake.URL+"/v1", "provided-secret", "")
	if cfg.NeedsAPIKey() || cfg.APIKey != "" {
		t.Fatal("keyless provider accepted a credential")
	}
	provider, err := cfg.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	msg, _, _, err := provider.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("ping")}, MaxTokens: 8})
	if err != nil || len(msg.Content) == 0 || msg.Content[0].Text != "OK" {
		t.Fatalf("completion=%+v err=%v", msg, err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestKeylessOpenAIRequiresExplicitSafeBaseURL(t *testing.T) {
	for _, base := range []string{"", "/v1", "ftp://example.com/v1", "https://user:password@example.com/v1", "https://example.com/v1?x=1"} {
		t.Run(base, func(t *testing.T) {
			cfg := agent.ConfigFrom("openai-keyless", "test-model", base, "", "")
			if _, err := cfg.NewProvider(); err == nil {
				t.Fatalf("accepted base %q", base)
			}
		})
	}
	t.Setenv("ARTEX_LLM_PROVIDER", "openai-keyless")
	t.Setenv("OPENAI_API_KEY", "cloud-secret")
	t.Setenv("ARTEX_LLM_MODEL", "test-model")
	t.Setenv("ARTEX_LLM_BASE_URL", "http://model.local/v1")
	cfg, ok := agent.FromEnv()
	if !ok || cfg.APIKey != "" || cfg.Provider() != "openai-keyless" {
		t.Fatalf("env config=%+v ok=%v", cfg, ok)
	}
	t.Setenv("ARTEX_LLM_BASE_URL", "")
	if _, ok := agent.FromEnv(); ok {
		t.Fatal("missing endpoint accepted")
	}
}

func TestKeylessOpenAIConnectionTestBoundsOutput(t *testing.T) {
	var maxTokens int
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MaxTokens int               `json:"max_tokens"`
			Tools     []json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		maxTokens = body.MaxTokens
		if len(body.Tools) != 0 {
			t.Errorf("connection test exposed %d tools", len(body.Tools))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer fake.Close()
	cfg := agent.ConfigFrom("openai-keyless", "test-model", fake.URL+"/v1", "", "")
	cfg.Stream = false
	cfg.ReasoningEffort = "none"
	if _, reply, err := agent.TestConnection(context.Background(), cfg); err != nil || reply != "OK" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if maxTokens != 128 {
		t.Fatalf("max_tokens=%d, want 128", maxTokens)
	}
}

func TestConnectionTestDoesNotExecuteModelToolCalls(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "unexpected-tool-execution")
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.Tools) != 0 {
			t.Errorf("connection test exposed %d tools", len(body.Tools))
		}
		args, _ := json.Marshal(map[string]string{"command": "touch " + marker})
		response := map[string]any{
			"id": "1", "object": "chat.completion", "model": "test-model",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{
				"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
					"id": "call1", "type": "function", "function": map[string]string{"name": "Bash", "arguments": string(args)},
				}},
			}, "finish_reason": "tool_calls"}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer fake.Close()
	cfg := agent.ConfigFrom("openai-keyless", "test-model", fake.URL+"/v1", "", "")
	cfg.Stream = false
	_, _, _ = agent.TestConnection(context.Background(), cfg)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("model tool call executed during connection test: %v", err)
	}
}

func TestKeylessOpenAIModelDiscoveryHasNoAuthorization(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "" {
			t.Errorf("path=%q authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
	}))
	defer fake.Close()
	body, _ := json.Marshal(map[string]any{"provider": "openai-keyless", "base_url": fake.URL + "/v1", "api_key": "must-not-send"})
	rec := httptest.NewRecorder()
	(&Server{}).pgListModels(rec, httptest.NewRequest(http.MethodPost, "/api/llm/models", bytes.NewReader(body)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "test-model") {
		t.Fatalf("model list: %d %s", rec.Code, rec.Body.String())
	}
}

func TestKeylessOpenAIProfileLifecycle(t *testing.T) {
	skipWithoutDSN(t)
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := New(ctx, m, t.TempDir(), t.TempDir(), t.TempDir())
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("keyless profile sent authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer fake.Close()
	pid, err := m.pg.SaveProfile(&db.LLMProfile{Name: "keyless-test", Format: "openai-keyless", Model: "test-model", BaseURL: fake.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec("DELETE FROM llm_profiles WHERE id=$1", pid) })
	if _, ok := s.loadProfileConfig(pid); !ok {
		t.Fatal("keyless profile did not load")
	}
	if err := s.validateTaskProfileIDs([]int64{pid}); err != nil {
		t.Fatal(err)
	}
	pool, err := m.pg.PoolProfiles()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range pool {
		found = found || p.ID == pid
	}
	if !found {
		t.Fatal("keyless profile missing from pool")
	}
	body, _ := json.Marshal(map[string]any{"profile_id": pid, "streaming": false})
	rec := httptest.NewRecorder()
	s.testLLM(rec, httptest.NewRequest(http.MethodPost, "/api/llm/test", bytes.NewReader(body)))
	var response struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || !response.OK {
		t.Fatalf("connection test: status=%d response=%s err=%v", rec.Code, rec.Body.String(), err)
	}
}
