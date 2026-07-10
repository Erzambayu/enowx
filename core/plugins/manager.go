package plugins

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"time"
)

const logRingSize = 400 // recent stdout/stderr lines kept per plugin

// Plugin is a manifest plus its live runtime state (for the API).
type Plugin struct {
	Manifest
	Running bool   `json:"running"`
	Port    int    `json:"port,omitempty"`
	Error   string `json:"error,omitempty"`
	HasIcon bool   `json:"has_icon"`
}

type proc struct {
	cmd     *exec.Cmd
	port    int
	running bool
	err     string
	logs    []string
}

// Manager runs and tracks plugin sidecar processes under the plugins dir.
type Manager struct {
	dir       string
	enowxPort int

	// gate, if set, authorizes running/serving a plugin (returns a non-nil error
	// to deny). Used to make plugins premium-only: it's checked before a process
	// is spawned and before a static plugin is served, so a shared plugin that's
	// installed manually still can't run without the entitlement.
	gate func() error

	mu    sync.Mutex
	procs map[string]*proc
}

func New(pluginsDir string, enowxPort int) *Manager {
	_ = os.MkdirAll(pluginsDir, 0o755)
	return &Manager{dir: pluginsDir, enowxPort: enowxPort, procs: map[string]*proc{}}
}

// SetGate installs the authorization check run before a plugin executes/serves.
func (m *Manager) SetGate(f func() error) { m.gate = f }

// authorized runs the gate (if any); nil means allowed.
func (m *Manager) authorized() error {
	if m.gate == nil {
		return nil
	}
	return m.gate()
}

// Dir returns the plugins root directory.
func (m *Manager) Dir() string { return m.dir }

// List returns every plugin folder's manifest + live state.
func (m *Manager) List() []Plugin {
	entries, _ := os.ReadDir(m.dir)
	out := []Plugin{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		man, err := readManifest(filepath.Join(m.dir, e.Name()))
		if err != nil {
			continue // skip folders without a valid manifest
		}
		p := Plugin{Manifest: *man, HasIcon: m.HasIcon(man.ID)}
		m.mu.Lock()
		if pr := m.procs[man.ID]; pr != nil {
			p.Running, p.Port, p.Error = pr.running, pr.port, pr.err
		}
		m.mu.Unlock()
		out = append(out, p)
	}
	return out
}

// Get returns a single plugin's manifest, or an error if unknown/invalid.
func (m *Manager) Get(id string) (*Manifest, error) {
	if !idRe.MatchString(id) {
		return nil, fmt.Errorf("invalid plugin id")
	}
	return readManifest(filepath.Join(m.dir, id))
}

// port returns the running port for a plugin, or 0.
func (m *Manager) port(id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if pr := m.procs[id]; pr != nil && pr.running {
		return pr.port
	}
	return 0
}

// envAllowList is what a plugin gets by default. LookupEnv simply misses
// whichever names don't apply to the current OS, so no runtime.GOOS branch
// is needed: PATH lets the plugin's own entry command find its runtime/
// tools; the rest are what node/python/go and Windows' network stack expect
// to find a home/cache/temp dir.
var envAllowList = []string{"PATH", "HOME", "USERPROFILE", "TMPDIR", "TEMP", "TMP", "SystemRoot"}

func minimalEnv() []string {
	env := make([]string, 0, len(envAllowList))
	for _, name := range envAllowList {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// buildEnv assembles a plugin's child-process environment. A plugin that
// declares "env:full" in Manifest.Permissions inherits the full parent
// environment (today's behavior); everything else gets envAllowList only,
// so it can't read ambient secrets out of enowX's own environment unless it
// explicitly opts in.
func buildEnv(man *Manifest, id string, port, enowxPort int) []string {
	env := minimalEnv()
	if slices.Contains(man.Permissions, "env:full") {
		env = os.Environ()
	}
	return append(env,
		"PORT="+strconv.Itoa(port),
		"ENOWX_PLUGIN_ID="+id,
		"ENOWX_API=http://127.0.0.1:"+strconv.Itoa(enowxPort),
	)
}

// Start spawns a plugin's sidecar (no-op for static plugins).
func (m *Manager) Start(id string) error {
	if err := m.authorized(); err != nil {
		return err
	}
	man, err := m.Get(id)
	if err != nil {
		return err
	}
	if man.Runtime == "static" {
		return fmt.Errorf("static plugins don't run a process")
	}
	m.mu.Lock()
	if pr := m.procs[id]; pr != nil && pr.running {
		m.mu.Unlock()
		return nil // already running
	}
	m.mu.Unlock()

	var bin string
	var args []string
	if man.Runtime == "bin" {
		// Prebuilt binary shipped with the plugin: resolve the per-OS/arch file
		// and run it directly (no toolchain needed).
		exe := filepath.Join(m.dir, id, resolveBinEntry(man.Entry))
		if _, err := os.Stat(exe); err != nil {
			return fmt.Errorf("no prebuilt binary for %s/%s", runtime.GOOS, runtime.GOARCH)
		}
		if runtime.GOOS != "windows" {
			_ = os.Chmod(exe, 0o755)
		}
		bin = exe
	} else {
		var ok bool
		bin, args, ok = runArgs(man.Runtime, man.Entry)
		if !ok {
			return fmt.Errorf("%s runtime is not installed", man.Runtime)
		}
	}
	port, err := freePort()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = filepath.Join(m.dir, id)
	cmd.Env = buildEnv(man, id, port, m.enowxPort)
	hideWindow(cmd)

	pr := &proc{cmd: cmd, port: port, running: true}
	m.mu.Lock()
	m.procs[id] = pr
	m.mu.Unlock()

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		m.mu.Lock()
		pr.running = false
		pr.err = err.Error()
		m.mu.Unlock()
		return err
	}
	go m.scan(pr, stdout)
	go m.scan(pr, stderr)
	// Reap + mark stopped when the process exits.
	go func() {
		werr := cmd.Wait()
		m.mu.Lock()
		pr.running = false
		if werr != nil && pr.err == "" {
			pr.err = werr.Error()
		}
		m.mu.Unlock()
	}()
	return nil
}

// Stop kills a running plugin process.
func (m *Manager) Stop(id string) {
	m.mu.Lock()
	pr := m.procs[id]
	m.mu.Unlock()
	if pr == nil || pr.cmd == nil || pr.cmd.Process == nil {
		return
	}
	_ = pr.cmd.Process.Kill()
	// Don't Wait() here — the reaper goroutine started in Start() owns the single
	// cmd.Wait(); killing the process makes it return and clean up.
	m.mu.Lock()
	pr.running = false
	m.mu.Unlock()
}

// Logs returns the recent stdout/stderr lines for a plugin.
func (m *Manager) Logs(id string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if pr := m.procs[id]; pr != nil {
		return append([]string(nil), pr.logs...)
	}
	return nil
}

// Delete stops the plugin and removes its folder.
func (m *Manager) Delete(id string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("invalid plugin id")
	}
	m.Stop(id)
	m.mu.Lock()
	delete(m.procs, id)
	m.mu.Unlock()
	return os.RemoveAll(filepath.Join(m.dir, id))
}

func (m *Manager) scan(pr *proc, r io.Reader) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 1<<20)
	for s.Scan() {
		line := time.Now().Format("15:04:05") + " " + s.Text()
		m.mu.Lock()
		pr.logs = append(pr.logs, line)
		if len(pr.logs) > logRingSize {
			pr.logs = pr.logs[len(pr.logs)-logRingSize:]
		}
		m.mu.Unlock()
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
