package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/enowdev/enowx/config"
	"github.com/enowdev/enowx/core/daemon"
	"github.com/enowdev/enowx/core/plugins"
)

// loadCfg loads the config or dies with a friendly message.
func loadCfg() config.Config {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	return cfg
}

// base returns the local server's base URL.
func base(cfg config.Config) string { return "http://127.0.0.1:" + itoa(cfg.Port) }

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// httpJSON does a request to the local server and unwraps the { data } envelope.
func httpJSON(method, url string) (map[string]any, int, error) {
	req, _ := http.NewRequest(method, url, nil)
	c := &http.Client{Timeout: 15 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var env struct {
		Data  map[string]any `json:"data"`
		Error string         `json:"error"`
	}
	_ = json.Unmarshal(b, &env)
	// Surface the server's error message (e.g. the update's "reinstall to
	// ~/.local/bin" guidance) as a real error so callers can print it verbatim.
	if resp.StatusCode >= 300 && env.Error != "" {
		return env.Data, resp.StatusCode, fmt.Errorf("%s", env.Error)
	}
	if env.Data == nil {
		// Some endpoints (e.g. /health) aren't enveloped.
		var raw map[string]any
		_ = json.Unmarshal(b, &raw)
		return raw, resp.StatusCode, nil
	}
	return env.Data, resp.StatusCode, nil
}

func serverUp(cfg config.Config) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(base(cfg) + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// --- commands --------------------------------------------------------------

func startCmd(args []string) {
	cfg := loadCfg()
	// Background by default; --foreground (-f) runs in this terminal (handy for
	// debugging or under a supervisor like systemd/launchd).
	fg := false
	for _, a := range args {
		if a == "--foreground" || a == "-f" {
			fg = true
		}
	}
	if fg {
		runServer()
		return
	}
	if serverUp(cfg) {
		fmt.Printf("enx is already running — dashboard %s\n", base(cfg))
		return
	}
	pid, err := daemon.Start(cfg.RuntimeDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("enx started (pid %d) — dashboard %s\n", pid, base(cfg))
}

func stopCmd() {
	cfg := loadCfg()
	if !daemon.IsRunning(cfg.RuntimeDir) {
		fmt.Println("enx is not running")
		return
	}
	if err := daemon.Stop(cfg.RuntimeDir); err != nil {
		fmt.Fprintf(os.Stderr, "stop: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("enx stopped")
}

func restartCmd(args []string) {
	cfg := loadCfg()
	if daemon.IsRunning(cfg.RuntimeDir) {
		_ = daemon.Stop(cfg.RuntimeDir)
	}
	// Restart always goes to the background (a restart of a foreground process
	// from another shell doesn't make sense).
	startCmd(nil)
	_ = args
}

func statusCmd() {
	cfg := loadCfg()
	if !serverUp(cfg) {
		fmt.Printf("● stopped — no server on %s\n", base(cfg))
		os.Exit(1)
	}
	pid, _ := daemon.GetPID(cfg.RuntimeDir)
	fmt.Printf("● running — %s\n", base(cfg))
	if pid > 0 {
		fmt.Printf("  pid       %d\n", pid)
	}
	if d, _, err := httpJSON(http.MethodGet, base(cfg)+"/api/debug"); err == nil {
		if v, ok := d["version"].(string); ok {
			fmt.Printf("  version   %s\n", v)
		}
		if b, ok := d["build"].(map[string]any); ok {
			if v, ok := b["version"].(string); ok {
				fmt.Printf("  version   %s\n", v)
			}
		}
		if up, ok := d["uptime"].(string); ok {
			fmt.Printf("  uptime    %s\n", up)
		}
	}
}

func doctorCmd() {
	cfg := loadCfg()
	fmt.Println("enx doctor")
	// Config dir writable.
	check("config dir writable", writable(cfg.RuntimeDir), cfg.RuntimeDir)
	// DB present.
	_, dbErr := os.Stat(cfg.DBPath())
	check("database", dbErr == nil, cfg.DBPath())
	// Server reachable.
	check("server running", serverUp(cfg), base(cfg)+"  (enx start)")
	// Runtimes for plugins.
	for _, r := range plugins.DetectRuntimes() {
		hint := "not installed"
		if r.Available {
			hint = r.Version
		}
		check("runtime "+r.ID, r.Available, hint)
	}
}

func check(label string, ok bool, hint string) {
	mark := "✗"
	if ok {
		mark = "✓"
	}
	fmt.Printf("  %s %-22s %s\n", mark, label, hint)
}

func writable(dir string) bool {
	f := dir + "/.enx-write-test"
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		return false
	}
	_ = os.Remove(f)
	return true
}

func updateCmd(args []string) {
	if version == "dev" {
		fmt.Println("this is a dev build — updates are disabled")
		return
	}
	cfg := loadCfg()
	checkOnly := false
	for _, a := range args {
		if a == "--check" {
			checkOnly = true
		}
	}
	if !serverUp(cfg) {
		fmt.Fprintln(os.Stderr, "enx must be running to check/apply updates (enx start)")
		os.Exit(1)
	}
	d, _, err := httpJSON(http.MethodGet, base(cfg)+"/api/version?fresh=1")
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		os.Exit(1)
	}
	avail, _ := d["update_available"].(bool)
	latest, _ := d["latest"].(string)
	if !avail {
		fmt.Printf("up to date (%s)\n", version)
		return
	}
	fmt.Printf("update available: %s → %s\n", version, latest)
	if checkOnly {
		return
	}
	fmt.Println("downloading + installing…")
	data, code, err := httpJSON(http.MethodPost, base(cfg)+"/api/update")
	if err != nil || code >= 300 {
		fmt.Fprintf(os.Stderr, "update failed (%d): %v\n", code, err)
		os.Exit(1)
	}
	if note, _ := data["note"].(string); note != "" {
		fmt.Println("\n" + note)
	}
	fmt.Println("update applied — enx will restart itself")
}

func tunnelCmd(args []string) {
	cfg := loadCfg()
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	if !serverUp(cfg) {
		fmt.Fprintln(os.Stderr, "enx must be running first (enx start)")
		os.Exit(1)
	}
	switch sub {
	case "start", "enable":
		fmt.Println("starting tunnel…")
		if _, code, err := httpJSON(http.MethodPost, base(cfg)+"/api/tunnel/enable"); err != nil || code >= 300 {
			fmt.Fprintf(os.Stderr, "tunnel: could not enable (%d): %v\n", code, err)
			os.Exit(1)
		}
		// Poll status for the public URL.
		for i := 0; i < 30; i++ {
			d, _, _ := httpJSON(http.MethodGet, base(cfg)+"/api/tunnel/status")
			if u, ok := d["url"].(string); ok && u != "" {
				fmt.Printf("\n  Public URL: %s\n\n", u)
				fmt.Println("  Open it in a browser. On first visit you'll be asked to")
				fmt.Println("  set a dashboard password before you can sign in.")
				return
			}
			time.Sleep(time.Second)
		}
		fmt.Println("tunnel enabled, but no URL yet — check `enx tunnel status`")
	case "stop", "disable":
		if _, _, err := httpJSON(http.MethodPost, base(cfg)+"/api/tunnel/disable"); err != nil {
			fmt.Fprintf(os.Stderr, "tunnel: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("tunnel stopped")
	case "status":
		d, _, _ := httpJSON(http.MethodGet, base(cfg)+"/api/tunnel/status")
		if u, ok := d["url"].(string); ok && u != "" {
			fmt.Printf("tunnel: on — %s\n", u)
		} else {
			fmt.Println("tunnel: off")
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown tunnel command %q (start|stop|status)\n", sub)
		os.Exit(1)
	}
}

func versionCmd() {
	fmt.Printf("enx %s\n", version)
	fmt.Printf("  %s/%s · %s\n", runtime.GOOS, runtime.GOARCH, runtime.Version())
}

func printHelp() {
	fmt.Print(`enx — enowx gateway

Usage:
  enx [start]           start the server in the background
  enx start -f          run in the foreground (this terminal)
  enx stop              stop the server
  enx restart           restart the server
  enx status            show whether the server is running
  enx doctor            check the environment (runtimes, config, server)
  enx update [--check]  self-update to the latest release
  enx tunnel start      expose the dashboard via a public URL
  enx tunnel stop       tear the tunnel down
  enx tunnel status     show the current tunnel URL
  enx skill install <slug> [-g]  install a skill (-g = global, else project)
  enx skill list                list installed skills
  enx skill remove <slug>       uninstall a skill
  enx skill update <slug>       reinstall a skill to the latest version
  enx version           print the version
`)
}
