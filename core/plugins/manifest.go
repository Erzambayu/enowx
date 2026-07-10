// Package plugins runs user-built mini-apps: each plugin is a folder under the
// runtime plugins/ dir with a plugin.json manifest + code in Go/Python/JS (or a
// static folder). enowx spawns it as a sidecar process and reverse-proxies its
// HTTP UI into the WebOS.
package plugins

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Manifest is a plugin's plugin.json.
type Manifest struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Icon        string   `json:"icon"`                  // lucide name, or "icon.png" in the folder
	Runtime     string   `json:"runtime"`               // go | python | node | static
	Entry       string   `json:"entry"`                 // command entry (ignored for static)
	UI          string   `json:"ui"`                    // path served at /plugins/<id>/ (default public/index.html)
	Version     string   `json:"version"`               // semver, bumped on each marketplace re-publish
	Permissions []string `json:"permissions,omitempty"` // "env:full" inherits the parent process env; default is a minimal allow-list (plugins.envAllowList)
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// validRuntimes are the runtimes a plugin may declare. "bin" ships a prebuilt
// executable per OS/arch (entry may contain {os}/{arch} placeholders), so it runs
// without any toolchain installed — used for official plugins in the marketplace.
var validRuntimes = map[string]bool{"go": true, "python": true, "node": true, "static": true, "bin": true}

// readManifest loads and validates a plugin.json from a plugin folder.
func readManifest(dir string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("plugin.json: %w", err)
	}
	if m.ID == "" {
		m.ID = filepath.Base(dir)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	if m.UI == "" {
		m.UI = "public/index.html"
	}
	if strings.TrimSpace(m.Version) == "" {
		m.Version = "1.0.0" // plugins predating versioning start at 1.0.0
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	if !idRe.MatchString(m.ID) {
		return fmt.Errorf("invalid plugin id %q (use lowercase letters, digits, dashes)", m.ID)
	}
	if !validRuntimes[m.Runtime] {
		return fmt.Errorf("invalid runtime %q (go|python|node|static)", m.Runtime)
	}
	if m.Runtime != "static" && strings.TrimSpace(m.Entry) == "" {
		return fmt.Errorf("entry is required for runtime %q", m.Runtime)
	}
	// Entry/UI must stay inside the plugin folder.
	for _, p := range []string{m.Entry, m.UI} {
		if p == "" {
			continue
		}
		if filepath.IsAbs(p) || strings.Contains(p, "..") {
			return fmt.Errorf("path escapes plugin folder: %q", p)
		}
	}
	return nil
}
