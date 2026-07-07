package integrations

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// readText reads a whole file as a string; ok=false when missing.
func readText(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// --- Claude Code: env block in ~/.claude/settings.json ---

var claudeEnvKeys = []string{
	"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_MODEL",
	"ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_SMALL_FAST_MODEL",
}

func applyClaude(base, key, model string) error {
	path := expand("~/.claude/settings.json")
	m, err := readJSON(path)
	if err != nil {
		return err
	}
	env := asMap(m, "env")
	env["ANTHROPIC_BASE_URL"] = anthropicBase(base) // CLI appends /v1/messages → /anthropic/v1/messages
	env["ANTHROPIC_AUTH_TOKEN"] = key
	env["ANTHROPIC_MODEL"] = model
	// Map the model to every tier so tier-specific requests route to us too.
	env["ANTHROPIC_DEFAULT_OPUS_MODEL"] = model
	env["ANTHROPIC_DEFAULT_SONNET_MODEL"] = model
	env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = model
	env["ANTHROPIC_SMALL_FAST_MODEL"] = model
	m["hasCompletedOnboarding"] = true
	return writeJSON(path, m)
}

func resetClaude() error {
	path := expand("~/.claude/settings.json")
	m, err := readJSON(path)
	if err != nil {
		return err
	}
	if env, ok := m["env"].(map[string]any); ok {
		for _, k := range claudeEnvKeys {
			delete(env, k)
		}
		if len(env) == 0 {
			delete(m, "env")
		}
	}
	return writeJSON(path, m)
}

// --- Codex: TOML config.toml + auth.json ---

func applyCodex(base, key, model string) error {
	cfgPath := expand("~/.codex/config.toml")
	txt, _ := readText(cfgPath)
	txt = stripCodexBlock(txt)
	block := fmt.Sprintf(`model = "%s"
model_provider = "enowx"

[model_providers.enowx]
name = "enowx"
base_url = "%s"
wire_api = "chat"
`, model, baseWithV1(base))
	txt = strings.TrimRight(txt, "\n")
	if txt != "" {
		txt += "\n\n"
	}
	txt += block
	if err := writeAtomic(cfgPath, []byte(txt)); err != nil {
		return err
	}
	// auth.json — OpenAI-style API key.
	authPath := expand("~/.codex/auth.json")
	auth, err := readJSON(authPath)
	if err != nil {
		return err
	}
	auth["OPENAI_API_KEY"] = key
	return writeJSON(authPath, auth)
}

var codexBlockRE = regexp.MustCompile(`(?ms)^\[model_providers\.enowx\].*?(?:\n\n|\z)`)

func stripCodexBlock(txt string) string {
	txt = codexBlockRE.ReplaceAllString(txt, "")
	// Drop our top-level model/model_provider lines.
	var keep []string
	for _, ln := range strings.Split(txt, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "model_provider = \"enowx\"") {
			continue
		}
		keep = append(keep, ln)
	}
	return strings.Join(keep, "\n")
}

func resetCodex() error {
	cfgPath := expand("~/.codex/config.toml")
	if txt, ok := readText(cfgPath); ok {
		_ = writeAtomic(cfgPath, []byte(strings.TrimSpace(stripCodexBlock(txt))+"\n"))
	}
	authPath := expand("~/.codex/auth.json")
	if auth, err := readJSON(authPath); err == nil {
		delete(auth, "OPENAI_API_KEY")
		_ = writeJSON(authPath, auth)
	}
	return nil
}

// --- OpenCode: provider map in opencode.json ---

func applyOpenCode(base, key string, models []string) error {
	path := expand("~/.config/opencode/opencode.json")
	m, err := readJSON(path)
	if err != nil {
		return err
	}
	prov := asMap(m, "provider")
	modelMap := map[string]any{}
	for _, mid := range models {
		modelMap[mid] = map[string]any{"name": mid}
	}
	prov["enowx"] = map[string]any{
		"npm":     "@ai-sdk/openai-compatible",
		"name":    "enowx",
		"options": map[string]any{"baseURL": baseWithV1(base), "apiKey": key},
		"models":  modelMap,
	}
	if len(models) > 0 {
		m["model"] = "enowx/" + models[0]
	}
	return writeJSON(path, m)
}

func resetOpenCode() error {
	path := expand("~/.config/opencode/opencode.json")
	m, err := readJSON(path)
	if err != nil {
		return err
	}
	if prov, ok := m["provider"].(map[string]any); ok {
		delete(prov, "enowx")
	}
	if s, _ := m["model"].(string); strings.HasPrefix(s, "enowx/") {
		delete(m, "model")
	}
	return writeJSON(path, m)
}

// --- Cline: globalState.json + secrets.json ---

func applyCline(base, key, model string) error {
	gs := expand("~/.cline/data/globalState.json")
	m, err := readJSON(gs)
	if err != nil {
		return err
	}
	m["apiProvider"] = "openai"
	m["openAiBaseUrl"] = baseNoV1(base) // Cline appends /v1
	m["openAiModelId"] = model
	if err := writeJSON(gs, m); err != nil {
		return err
	}
	sec := expand("~/.cline/data/secrets.json")
	s, err := readJSON(sec)
	if err != nil {
		return err
	}
	s["openAiApiKey"] = key
	return writeJSON(sec, s)
}

func resetCline() error {
	gs := expand("~/.cline/data/globalState.json")
	if m, err := readJSON(gs); err == nil {
		for _, k := range []string{"apiProvider", "openAiBaseUrl", "openAiModelId"} {
			delete(m, k)
		}
		_ = writeJSON(gs, m)
	}
	sec := expand("~/.cline/data/secrets.json")
	if s, err := readJSON(sec); err == nil {
		delete(s, "openAiApiKey")
		_ = writeJSON(sec, s)
	}
	return nil
}

// --- Kilo Code: auth.json block ---

func applyKilo(base, key, model string) error {
	path := expand("~/.local/share/kilo/auth.json")
	m, err := readJSON(path)
	if err != nil {
		return err
	}
	m["openai-compatible"] = map[string]any{
		"type": "api-key", "apiKey": key, "baseUrl": baseWithV1(base), "model": model,
	}
	return writeJSON(path, m)
}

func resetKilo() error {
	path := expand("~/.local/share/kilo/auth.json")
	if m, err := readJSON(path); err == nil {
		delete(m, "openai-compatible")
		_ = writeJSON(path, m)
	}
	return nil
}

// --- Factory Droid: customModels[] in settings.json ---

func applyDroid(base, key string, models []string) error {
	path := expand("~/.factory/settings.json")
	m, err := readJSON(path)
	if err != nil {
		return err
	}
	// Keep non-enowx entries, replace ours.
	var kept []any
	if arr, ok := m["customModels"].([]any); ok {
		for _, e := range arr {
			if em, ok := e.(map[string]any); ok {
				if bu, _ := em["baseUrl"].(string); strings.Contains(strings.ToLower(bu), "localhost") || strings.Contains(strings.ToLower(bu), "127.0.0.1") {
					continue
				}
			}
			kept = append(kept, e)
		}
	}
	for _, mid := range models {
		kept = append(kept, map[string]any{
			"modelDisplayName": "enowx/" + mid,
			"model":            mid,
			"baseUrl":          baseWithV1(base),
			"apiKey":           key,
			"provider":         "generic-chat-completion-api",
		})
	}
	m["customModels"] = kept
	return writeJSON(path, m)
}

func resetDroid() error {
	path := expand("~/.factory/settings.json")
	m, err := readJSON(path)
	if err != nil {
		return err
	}
	var kept []any
	if arr, ok := m["customModels"].([]any); ok {
		for _, e := range arr {
			if em, ok := e.(map[string]any); ok {
				if bu, _ := em["baseUrl"].(string); strings.Contains(strings.ToLower(bu), "localhost") || strings.Contains(strings.ToLower(bu), "127.0.0.1") {
					continue
				}
			}
			kept = append(kept, e)
		}
	}
	m["customModels"] = kept
	return writeJSON(path, m)
}

// --- Open Claw: providers map in openclaw.json ---

func applyOpenClaw(base, key string, models []string) error {
	path := expand("~/.openclaw/openclaw.json")
	m, err := readJSON(path)
	if err != nil {
		return err
	}
	prov := asMap(m, "providers")
	prov["enowx"] = map[string]any{
		"api":     "openai-completions",
		"baseURL": baseWithV1(base),
		"apiKey":  key,
		"models":  models,
	}
	return writeJSON(path, m)
}

func resetOpenClaw() error {
	path := expand("~/.openclaw/openclaw.json")
	if m, err := readJSON(path); err == nil {
		if prov, ok := m["providers"].(map[string]any); ok {
			delete(prov, "enowx")
		}
		_ = writeJSON(path, m)
	}
	return nil
}

// --- Hermes: config.yaml + .env ---

func applyHermes(base, key, model string) error {
	yaml := expand("~/.hermes/config.yaml")
	content := fmt.Sprintf("model: %s\nbase_url: %s\n", model, baseWithV1(base))
	if err := writeAtomic(yaml, []byte(content)); err != nil {
		return err
	}
	env := expand("~/.hermes/.env")
	return writeAtomic(env, []byte(fmt.Sprintf("OPENAI_API_KEY=%s\n", key)))
}

func resetHermes() error {
	_ = os.Remove(expand("~/.hermes/config.yaml"))
	_ = os.Remove(expand("~/.hermes/.env"))
	return nil
}
