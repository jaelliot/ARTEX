package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/Autumn-27/artex/internal/worker"
	"github.com/Autumn-27/norma/agentcore"
	actool "github.com/Autumn-27/norma/tool"
)

// offlineWorkerOptions is deliberately narrow: every model-invoked command
// goes through the broker, and SDK-injected network and background tools are
// disabled. It does not yet provide persistent task files or custom tools.
func offlineWorkerOptions(opts agentcore.Options) agentcore.Options {
	socket, enabled := worker.SocketPath()
	if !enabled {
		return opts
	}
	tools := make([]actool.CoreTool, 0, 1)
	for _, tool := range opts.Tools {
		if tool.Name() == "Bash" {
			tools = append(tools, &offlineBash{CoreTool: tool, socket: socket})
		}
	}
	opts.Tools = tools // non-nil even when no Bash tool was enabled
	opts.DeferredTools, opts.UnlockSet = nil, nil
	opts.Plan, opts.Memory, opts.Skills = nil, nil, nil
	opts.EnableWebFetch, opts.EnableWebSearch = false, false
	opts.Todos, opts.AskUser = nil, nil
	opts.DisableBackgroundTasks = true
	opts.SystemPrompt = append(append([]string{}, opts.SystemPrompt...),
		"Offline worker mode: Bash runs in a fresh isolated container for each call. It has no network access; files do not persist between calls. Other tools are unavailable.")
	return opts
}

func newAgentSession(opts agentcore.Options) *agentcore.Session {
	return agentcore.NewSession(offlineWorkerOptions(opts))
}

type offlineBash struct {
	actool.CoreTool
	socket string
}

func (b *offlineBash) Description() string {
	return "Run a Bash command in a fresh offline container. Files do not persist between calls."
}

func (b *offlineBash) Prompt() string {
	return "Commands have no network or host access, run for at most 120 seconds, and cannot run in the background."
}

func (b *offlineBash) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"command":    map[string]any{"type": "string"},
		"timeout_ms": map[string]any{"type": "integer"},
	}, "required": []any{"command"}, "additionalProperties": false}
}

func (b *offlineBash) Call(ctx context.Context, raw json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	var input struct {
		Command   string `json:"command"`
		TimeoutMS int    `json:"timeout_ms"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || strings.TrimSpace(input.Command) == "" {
		return actool.Errorf("offline worker requires a command and does not support background execution"), nil
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return actool.Errorf("invalid offline worker request"), nil
	}
	timeout := 120 * time.Second
	if input.TimeoutMS > 0 && input.TimeoutMS < 120000 {
		timeout = time.Duration(input.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := worker.Run(ctx, b.socket, input.Command)
	if err != nil {
		return actool.Errorf("offline worker unavailable: " + err.Error()), nil
	}
	if !result.OK {
		return actool.Errorf(actool.Capture(tc, strings.TrimSpace(result.Output+"\n"+result.Error))), nil
	}
	return actool.Text(actool.Capture(tc, result.Output)), nil
}
