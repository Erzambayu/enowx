// Package integrations connects local CLI coding agents (Claude Code, Codex,
// Cline, …) to this gateway by writing their config files to point at the
// gateway's OpenAI/Anthropic-compatible endpoint with the user's API key.
//
// Each tool is described by a Spec and handled by an apply/status/reset trio.
// enx runs on the user's machine, so the primary path writes the config directly
// (atomic, merge-only — only fields we own are touched); a snippet generator
// covers remote/undetected setups.
package integrations

import (
	"os"
	"path/filepath"
	"strings"
)

// Spec is static metadata about a supported tool.
type Spec struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Binary string `json:"binary"` // command probed on PATH to detect "installed"
	// ConfigPaths are the files this tool's config lives in (for display + the
	// "installed via config" fallback when the binary isn't on PATH).
	ConfigPaths []string `json:"config_paths"`
	// MultiModel is true when the tool accepts a list of models rather than one.
	MultiModel bool `json:"multi_model"`
	// Anthropic tools auto-append /v1/messages, so their base URL must NOT carry
	// /v1; OpenAI-compatible tools want the /v1 suffix.
	Anthropic bool `json:"anthropic"`
}

// Status is the live state of a tool on this machine.
type Status struct {
	Spec
	Installed bool     `json:"installed"`
	Connected bool     `json:"connected"` // config currently points at this gateway
	Paths     []string `json:"paths"`     // resolved absolute config paths
	Models    []string `json:"models"`    // models currently configured for us
	Message   string   `json:"message,omitempty"`
}

// ApplyRequest is the payload to connect a tool.
type ApplyRequest struct {
	BaseURL string   `json:"base_url"` // gateway base, e.g. http://localhost:1430
	APIKey  string   `json:"api_key"`
	Model   string   `json:"model"`
	Models  []string `json:"models"`
}

// Snippet is one config file to write (for the copy-paste / remote path).
type Snippet struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Format  string `json:"format"` // json | toml | yaml | env
}

// specs is the ordered registry of supported tools.
var specs = []Spec{
	{Key: "claude", Name: "Claude Code", Binary: "claude", Anthropic: true,
		ConfigPaths: []string{"~/.claude/settings.json"}},
	{Key: "codex", Name: "Codex", Binary: "codex",
		ConfigPaths: []string{"~/.codex/config.toml", "~/.codex/auth.json"}},
	{Key: "opencode", Name: "OpenCode", Binary: "opencode", MultiModel: true,
		ConfigPaths: []string{"~/.config/opencode/opencode.json"}},
	{Key: "cline", Name: "Cline", Binary: "cline",
		ConfigPaths: []string{"~/.cline/data/globalState.json", "~/.cline/data/secrets.json"}},
	{Key: "kilo", Name: "Kilo Code", Binary: "kilo",
		ConfigPaths: []string{"~/.local/share/kilo/auth.json"}},
	{Key: "droid", Name: "Factory Droid", Binary: "droid", MultiModel: true,
		ConfigPaths: []string{"~/.factory/settings.json"}},
	{Key: "openclaw", Name: "Open Claw", Binary: "openclaw", MultiModel: true,
		ConfigPaths: []string{"~/.openclaw/openclaw.json"}},
	{Key: "hermes", Name: "Hermes", Binary: "hermes",
		ConfigPaths: []string{"~/.hermes/config.yaml", "~/.hermes/.env"}},
}

// Specs returns the registry in display order.
func Specs() []Spec { return append([]Spec(nil), specs...) }

// SpecByKey looks up a tool spec.
func SpecByKey(key string) (Spec, bool) {
	for _, s := range specs {
		if s.Key == key {
			return s, true
		}
	}
	return Spec{}, false
}

// --- path + URL helpers ---

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

// expand resolves a leading ~ to the user's home directory.
func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home(), p[2:])
	}
	return p
}

// baseWithV1 normalizes a gateway base URL to end with /v1 (OpenAI-compatible).
func baseWithV1(base string) string {
	b := strings.TrimRight(strings.TrimSpace(base), "/")
	if b == "" {
		b = "http://localhost:1430"
	}
	if !strings.HasSuffix(b, "/v1") {
		b += "/v1"
	}
	return b
}

// baseNoV1 strips a trailing /v1 (Anthropic tools append their own path).
func baseNoV1(base string) string {
	b := strings.TrimRight(strings.TrimSpace(base), "/")
	if b == "" {
		b = "http://localhost:1430"
	}
	return strings.TrimSuffix(b, "/v1")
}

// anthropicBase returns the gateway's Anthropic-compatible base for Claude Code:
// the gateway root + "/anthropic". Claude Code appends "/v1/messages", so this
// yields "<gateway>/anthropic/v1/messages", which is where enx serves it (a plain
// "<gateway>/v1/messages" is NOT the Anthropic endpoint).
func anthropicBase(base string) string {
	b := baseNoV1(base)
	if strings.HasSuffix(b, "/anthropic") {
		return b
	}
	return b + "/anthropic"
}

// isOurBase reports whether a base URL already points at a gateway of ours: a
// localhost/loopback host, or a value that matches the configured base.
func isOurBase(cfgURL, ourBase string) bool {
	c := strings.ToLower(strings.TrimSpace(cfgURL))
	if c == "" {
		return false
	}
	for _, h := range []string{"localhost", "127.0.0.1", "0.0.0.0", "[::1]"} {
		if strings.Contains(c, h) {
			return true
		}
	}
	ob := strings.ToLower(strings.TrimRight(strings.TrimSuffix(strings.TrimRight(ourBase, "/"), "/v1"), "/"))
	return ob != "" && strings.Contains(c, ob)
}

// isOurKey reports whether an API key is one we issued (enx- prefix).
func isOurKey(k string) bool { return strings.HasPrefix(strings.TrimSpace(k), "enx-") }
