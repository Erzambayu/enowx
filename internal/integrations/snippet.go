package integrations

import (
	"encoding/json"
	"fmt"
)

// Snippets returns the config file(s) a tool needs, as copy-paste content — for
// setting a tool up on a remote machine the gateway can't write to directly.
func Snippets(s Spec, req ApplyRequest) []Snippet {
	models := req.Models
	if len(models) == 0 && req.Model != "" {
		models = []string{req.Model}
	}
	model := ""
	if len(models) > 0 {
		model = models[0]
	}
	j := func(v any) string {
		b, _ := json.MarshalIndent(v, "", "  ")
		return string(b)
	}
	switch s.Key {
	case "claude":
		return []Snippet{{Path: "~/.claude/settings.json", Format: "json", Content: j(map[string]any{
			"env": map[string]any{
				"ANTHROPIC_BASE_URL": anthropicBase(req.BaseURL), "ANTHROPIC_AUTH_TOKEN": req.APIKey,
				"ANTHROPIC_MODEL": model, "ANTHROPIC_DEFAULT_OPUS_MODEL": model,
				"ANTHROPIC_DEFAULT_SONNET_MODEL": model, "ANTHROPIC_DEFAULT_HAIKU_MODEL": model,
			},
			"hasCompletedOnboarding": true,
		})}}
	case "codex":
		return []Snippet{
			{Path: "~/.codex/config.toml", Format: "toml", Content: fmt.Sprintf(
				"model = \"%s\"\nmodel_provider = \"enowx\"\n\n[model_providers.enowx]\nname = \"enowx\"\nbase_url = \"%s\"\nwire_api = \"chat\"\n",
				model, baseWithV1(req.BaseURL))},
			{Path: "~/.codex/auth.json", Format: "json", Content: j(map[string]any{"OPENAI_API_KEY": req.APIKey})},
		}
	case "opencode":
		mm := map[string]any{}
		for _, mid := range models {
			mm[mid] = map[string]any{"name": mid}
		}
		return []Snippet{{Path: "~/.config/opencode/opencode.json", Format: "json", Content: j(map[string]any{
			"provider": map[string]any{"enowx": map[string]any{
				"npm": "@ai-sdk/openai-compatible", "name": "enowx",
				"options": map[string]any{"baseURL": baseWithV1(req.BaseURL), "apiKey": req.APIKey}, "models": mm,
			}},
			"model": "enowx/" + model,
		})}}
	case "cline":
		return []Snippet{
			{Path: "~/.cline/data/globalState.json", Format: "json", Content: j(map[string]any{
				"apiProvider": "openai", "openAiBaseUrl": baseNoV1(req.BaseURL), "openAiModelId": model})},
			{Path: "~/.cline/data/secrets.json", Format: "json", Content: j(map[string]any{"openAiApiKey": req.APIKey})},
		}
	case "kilo":
		return []Snippet{{Path: "~/.local/share/kilo/auth.json", Format: "json", Content: j(map[string]any{
			"openai-compatible": map[string]any{"type": "api-key", "apiKey": req.APIKey, "baseUrl": baseWithV1(req.BaseURL), "model": model}})}}
	case "droid":
		var cm []any
		for _, mid := range models {
			cm = append(cm, map[string]any{"modelDisplayName": "enowx/" + mid, "model": mid,
				"baseUrl": baseWithV1(req.BaseURL), "apiKey": req.APIKey, "provider": "generic-chat-completion-api"})
		}
		return []Snippet{{Path: "~/.factory/settings.json", Format: "json", Content: j(map[string]any{"customModels": cm})}}
	case "openclaw":
		return []Snippet{{Path: "~/.openclaw/openclaw.json", Format: "json", Content: j(map[string]any{
			"providers": map[string]any{"enowx": map[string]any{
				"api": "openai-completions", "baseURL": baseWithV1(req.BaseURL), "apiKey": req.APIKey, "models": models}}})}}
	case "hermes":
		return []Snippet{
			{Path: "~/.hermes/config.yaml", Format: "yaml", Content: fmt.Sprintf("model: %s\nbase_url: %s\n", model, baseWithV1(req.BaseURL))},
			{Path: "~/.hermes/.env", Format: "env", Content: "OPENAI_API_KEY=" + req.APIKey + "\n"},
		}
	}
	return nil
}
