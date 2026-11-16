package server

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/Autumn-27/artex/internal/agent"
	"github.com/Autumn-27/artex/internal/db"
)

// getLLM returns the current LLM config (key never exposed).
func (s *Server) getLLM(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	writeJSON(w, 200, map[string]any{
		"configured":       s.llmOn,
		"provider":         s.llmCfg.Provider(),
		"model":            s.llmCfg.Model,
		"base_url":         s.llmCfg.BaseURL,
		"proxy":            maskedSecret(s.llmCfg.Proxy),
		"proxy_set":        s.llmCfg.Proxy != "",
		"key_set":          s.llmCfg.APIKey != "",
		"rate_per_second":  s.llmCfg.RatePerSecond,
		"rate_per_minute":  s.llmCfg.RatePerMinute,
		"context_window_k": s.llmCfg.ContextWindowK,
		"thinking_type":    s.llmCfg.ThinkingType,
		"reasoning_effort": s.llmCfg.ReasoningEffort,
	})
}

// setLLM configures the LLM at runtime. A blank api_key keeps the existing key.
func (s *Server) setLLM(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider        string  `json:"provider"`
		Model           string  `json:"model"`
		BaseURL         string  `json:"base_url"`
		Proxy           string  `json:"proxy"`
		APIKey          string  `json:"api_key"`
		RatePerSecond   float64 `json:"rate_per_second"`
		RatePerMinute   float64 `json:"rate_per_minute"`
		ContextWindowK  int     `json:"context_window_k"`
		ThinkingType    string  `json:"thinking_type"`
		ReasoningEffort string  `json:"reasoning_effort"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if isMaskedSecret(req.Proxy) { // getLLM returns the proxy masked: keep the current one
		s.cfgMu.Lock()
		req.Proxy = s.llmCfg.Proxy
		s.cfgMu.Unlock()
	}
	cfg := agent.ConfigFrom(req.Provider, req.Model, req.BaseURL, req.APIKey, req.Proxy)
	cfg.RatePerSecond, cfg.RatePerMinute = req.RatePerSecond, req.RatePerMinute
	cfg.ThinkingType = req.ThinkingType
	cfg.ReasoningEffort = req.ReasoningEffort
	if k := req.ContextWindowK; k > 0 { // 0 = keep default (200K); cap at 1M
		if k > 1000 {
			k = 1000
		}
		cfg.ContextWindowK = k
	}
	// Keyless formats (Ollama) need no key; the others keep/require one.
	if cfg.APIKey == "" && cfg.NeedsAPIKey() {
		s.cfgMu.Lock()
		cfg.APIKey = s.llmCfg.APIKey // keep existing key if not re-entered
		s.cfgMu.Unlock()
	}
	if cfg.APIKey == "" && cfg.NeedsAPIKey() {
		writeErr(w, 400, "api_key required")
		return
	}
	// Validate provider construction before persisting it as the active profile.
	if _, err := cfg.NewProvider(); err != nil {
		writeErr(w, 400, "provider init failed: "+err.Error())
		return
	}
	if err := s.saveLLMConfig(cfg); err != nil {
		writeErr(w, 500, "persist provider failed: "+err.Error())
		return
	}
	s.invalidateProfileAgents()
	if _, ok := s.loadLLMConfig(); !ok {
		writeErr(w, 500, "saved provider is unavailable")
		return
	}
	if err := s.applyLLM(cfg); err != nil {
		writeErr(w, 400, "provider init failed: "+err.Error())
		return
	}
	log.Printf("[engine] LLM configured via UI: %s / %s", cfg.Provider(), cfg.Model)
	s.getLLM(w, r)
}

// testLLM makes a real minimal completion to verify the config works.
func (s *Server) testLLM(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider         string `json:"provider"`
		Model            string `json:"model"`
		BaseURL          string `json:"base_url"`
		Proxy            string `json:"proxy"`
		APIKey           string `json:"api_key"`
		ThinkingType     string `json:"thinking_type"`
		ReasoningEffort  string `json:"reasoning_effort"`
		ProfileID        *int64 `json:"profile_id"`         // passed when testing a stored profile: if api_key is empty, its stored key is used
		Streaming        *bool  `json:"streaming"`          // omitted = streaming, the same defaults as when saving a profile
		SessionHeaderKey string `json:"session_header_key"` // non-empty = the test also sends this custom session header, with a one-off random session id as the value
		UserAgent        string `json:"user_agent"`         // non-empty = the test overrides the User-Agent header with this value (mirrors production)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// A stored profile must be tested AS STORED: the list-view test button
	// sends only profile_id, and ConfigFrom's defaults for an empty
	// provider/base_url point at the canonical provider endpoint, sending this
	// profile's key to the wrong host (the stored-key 401s). Fill whatever the
	// form left empty from the row first; explicit form values still win.
	var stored *db.LLMProfile
	if req.ProfileID != nil {
		if p, err := s.m.pg.ProfileByID(*req.ProfileID); err == nil && p != nil {
			stored = p
		}
	}
	if isMaskedSecret(req.Proxy) { // the form echoes the masked proxy: test with the stored one
		req.Proxy = ""
		if stored == nil {
			s.cfgMu.Lock()
			req.Proxy = s.llmCfg.Proxy
			s.cfgMu.Unlock()
		}
	}
	if stored != nil {
		if strings.TrimSpace(req.Provider) == "" {
			req.Provider = stored.Format
		}
		if strings.TrimSpace(req.Model) == "" {
			req.Model = stored.Model
		}
		if strings.TrimSpace(req.BaseURL) == "" {
			req.BaseURL = stored.BaseURL
		}
		if strings.TrimSpace(req.Proxy) == "" {
			req.Proxy = stored.Proxy
		}
		if req.Streaming == nil {
			st := stored.Streaming
			req.Streaming = &st
		}
		if strings.TrimSpace(req.ThinkingType) == "" {
			req.ThinkingType = stored.ThinkingType
		}
		if strings.TrimSpace(req.ReasoningEffort) == "" {
			req.ReasoningEffort = stored.ReasoningEffort
		}
	}
	cfg := agent.ConfigFrom(req.Provider, req.Model, req.BaseURL, req.APIKey, req.Proxy)
	// mirror production: send the SAME thinking params so a provider that rejects the
	// reasoning_effort/thinking field fails the test too (no false "test ok, run 400").
	cfg.ThinkingType = req.ThinkingType
	cfg.ReasoningEffort = req.ReasoningEffort
	// Likewise, the streaming mode follows that profile's choice: endpoints supporting only one of the two channels must fail
	// here already, instead of discovering mid-conversation that "the profile that passed the test cannot run at all".
	if req.Streaming != nil {
		cfg.Stream = *req.Streaming
	}
	// The custom session header name follows that profile too: when non-empty, the test request also sends it (value is a one-off random session id,
	// see TestConnection). Endpoints like opencode zen that mandate x-opencode-session return
	// 400 without it, so the test path must send it too — otherwise "chat works, test 400s".
	cfg.SessionHeaderKey = req.SessionHeaderKey
	// The custom session header name follows that profile regardless of format:
	// when the form omits it, the stored profile's value is the fallback.
	if stored != nil && cfg.SessionHeaderKey == "" {
		cfg.SessionHeaderKey = strings.TrimSpace(stored.SessionHeaderKey)
	}
	// The custom User-Agent follows the profile too, so the test request uses the
	// exact UA a real conversation would; the form value wins, else the stored one.
	cfg.UserAgent = strings.TrimSpace(req.UserAgent)
	if stored != nil && cfg.UserAgent == "" {
		cfg.UserAgent = strings.TrimSpace(stored.UserAgent)
	}
	// Same rule as saving a profile: a value with control characters or over the
	// length cap never reaches the wire, not even from the test button.
	ua, err := agent.ValidateUserAgent(cfg.UserAgent)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	cfg.UserAgent = ua
	// Keyless formats (Ollama) skip the key resolution entirely.
	if cfg.NeedsAPIKey() {
		// API key resolution precedence: form input > the named profile's stored key > the global config's key.
		// A stored profile's key is never sent back to the browser, so the form is empty when testing a stored profile and the key comes from the DB.
		// A saved key is only sent to the endpoint it was saved for: base_url and
		// provider come from the form, so without this check the test button sends
		// the saved key to any host typed into the form.
		withheld := false
		if stored != nil && cfg.APIKey == "" {
			if sameLLMEndpoint(req.Provider, req.BaseURL, stored.Format, stored.BaseURL) {
				// Trim like ConfigFrom does for form values: a key stored with
				// surrounding whitespace runs fine in conversations (the runtime
				// path re-trims) but would be sent raw here and fail auth.
				cfg.APIKey = strings.TrimSpace(stored.APIKey)
			} else {
				withheld = true
			}
		}
		if cfg.APIKey == "" {
			s.cfgMu.Lock()
			global := s.llmCfg
			s.cfgMu.Unlock()
			if global.APIKey != "" {
				if sameLLMEndpoint(req.Provider, req.BaseURL, global.Provider(), global.BaseURL) {
					cfg.APIKey = global.APIKey
				} else {
					withheld = true
				}
			}
		}
		if cfg.APIKey == "" {
			msg := "no API key provided"
			if withheld {
				msg = "no API key provided: the saved key is only sent to the endpoint it was saved for; enter the key to test a different base URL or provider"
			}
			writeJSON(w, 200, map[string]any{"ok": false, "error": msg})
			return
		}
	}
	// Retry params do not go into the bounded connection test; stacking configured retry counts and intervals
	// would only turn a perfectly usable endpoint into a "timeout failure". The test asks "does this endpoint work at all"; retry pacing
	// is a runtime concern.
	lat, reply, err := agent.TestConnection(r.Context(), cfg)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// Return the model's actual reply so a "passed" test has evidence: you can see it really spoke, not just an HTTP 200.
	writeJSON(w, 200, map[string]any{
		"ok": true, "latency_ms": lat.Milliseconds(), "model": cfg.Model, "reply": truncateReply(reply),
	})
}

// sameLLMEndpoint reports whether two provider/base URL pairs resolve to the same
// endpoint after ConfigFrom's normalization (trimmed, endpoint suffix stripped;
// an empty base URL means the provider's canonical host). A saved API key may only
// be sent when the request's endpoint matches the one it was saved for.
func sameLLMEndpoint(provA, baseA, provB, baseB string) bool {
	a := agent.ConfigFrom(provA, "", baseA, "", "")
	b := agent.ConfigFrom(provB, "", baseB, "", "")
	return a.Format == b.Format && a.BaseURL == b.BaseURL
}

// truncateReply clips a connection-test reply for display. A model told to answer
// "OK" can still ramble (or think out loud); the UI only needs enough to show it
// really said something. Rune-based so multibyte text never splits mid-character.
func truncateReply(s string) string {
	const max = 200
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
