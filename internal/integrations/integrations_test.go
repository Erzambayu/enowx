package integrations

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Claude Code appends /v1/messages, so ANTHROPIC_BASE_URL must carry /anthropic
// (enx serves the Anthropic endpoint at /anthropic/v1/messages, not /v1/messages).
func TestClaudeBaseHasAnthropicPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	spec, _ := SpecByKey("claude")
	cases := map[string]string{
		"http://localhost:1430":           "http://localhost:1430/anthropic",
		"http://localhost:1430/v1":        "http://localhost:1430/anthropic",
		"https://x.trycloudflare.com":     "https://x.trycloudflare.com/anthropic",
		"http://localhost:1430/anthropic": "http://localhost:1430/anthropic",
	}
	for in, want := range cases {
		if err := Apply(spec, ApplyRequest{BaseURL: in, APIKey: "enx-x", Model: "clc/claude-sonnet-5"}); err != nil {
			t.Fatalf("apply %s: %v", in, err)
		}
		b, _ := os.ReadFile(filepath.Join(tmp, ".claude/settings.json"))
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		env, _ := m["env"].(map[string]any)
		if got, _ := env["ANTHROPIC_BASE_URL"].(string); got != want {
			t.Errorf("base %q → ANTHROPIC_BASE_URL=%q, want %q", in, got, want)
		}
	}
}

// applyClaude → connected → reset, against a temp HOME.
func TestClaudeRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	spec, _ := SpecByKey("claude")
	base := "http://localhost:1430"
	if err := Apply(spec, ApplyRequest{BaseURL: base, APIKey: "enx-test", Model: "clc/claude-opus-4-8"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// File exists + points at us.
	if !fileExists(filepath.Join(tmp, ".claude/settings.json")) {
		t.Fatal("settings.json not written")
	}
	if st := StatusOf(spec, base); !st.Connected {
		t.Fatalf("expected connected after apply")
	}
	if err := Reset(spec); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if st := StatusOf(spec, base); st.Connected {
		t.Fatalf("expected disconnected after reset")
	}
}

// Apply merges: a pre-existing unrelated key survives.
func TestClaudeMergePreservesOtherKeys(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	path := filepath.Join(tmp, ".claude/settings.json")
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte(`{"theme":"dark","env":{"MY_VAR":"keep"}}`), 0o600)

	spec, _ := SpecByKey("claude")
	Apply(spec, ApplyRequest{BaseURL: "http://localhost:1430", APIKey: "enx-x", Model: "m"})
	m, _ := readJSON(path)
	if m["theme"] != "dark" {
		t.Fatal("unrelated top-level key lost")
	}
	env, _ := m["env"].(map[string]any)
	if env["MY_VAR"] != "keep" {
		t.Fatal("unrelated env var lost")
	}
	// Reset leaves the unrelated env var.
	Reset(spec)
	m, _ = readJSON(path)
	env, _ = m["env"].(map[string]any)
	if env["MY_VAR"] != "keep" {
		t.Fatal("reset clobbered unrelated env var")
	}
}

// Multi-model tools (opencode) write every model.
func TestOpenCodeMultiModel(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	spec, _ := SpecByKey("opencode")
	Apply(spec, ApplyRequest{BaseURL: "http://localhost:1430", APIKey: "enx-x", Models: []string{"a", "b"}})
	st := StatusOf(spec, "http://localhost:1430")
	if !st.Connected || len(st.Models) != 2 {
		t.Fatalf("want connected with 2 models, got connected=%v models=%v", st.Connected, st.Models)
	}
}
