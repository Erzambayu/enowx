// Package config loads the one typed Config at startup (JSON file + env
// overrides), passed explicitly — no package-level globals.
package config

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
)

type Config struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	RuntimeDir string `json:"runtime_dir"`
	LogLevel   string `json:"log_level"`
}

func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Host:       "127.0.0.1",
		Port:       1430,
		RuntimeDir: filepath.Join(home, ".enowx"),
		LogLevel:   "info",
	}
}

func Load() (Config, error) {
	c := Default()
	if v := os.Getenv("ENOWX_RUNTIME_DIR"); v != "" {
		c.RuntimeDir = v
	}
	if b, err := os.ReadFile(filepath.Join(c.RuntimeDir, "config.json")); err == nil {
		if err := json.Unmarshal(b, &c); err != nil {
			log.Printf("config: %s/config.json: ignoring malformed JSON: %v", c.RuntimeDir, err)
		}
	}
	if v := os.Getenv("ENOWX_HOST"); v != "" {
		c.Host = v
	}
	if v := os.Getenv("ENOWX_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Port = n
		}
	}
	if v := os.Getenv("ENOWX_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if err := os.MkdirAll(c.RuntimeDir, 0o755); err != nil {
		return c, err
	}
	return c, nil
}

func (c Config) DBPath() string     { return filepath.Join(c.RuntimeDir, "enowx.db") }
func (c Config) PluginsDir() string { return filepath.Join(c.RuntimeDir, "plugins") }
func (c Config) Addr() string       { return c.Host + ":" + strconv.Itoa(c.Port) }
