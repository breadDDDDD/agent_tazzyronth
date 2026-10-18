package hub

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The registry is a machine-wide list of running hubs, one file per project
// in ~/.tazzyronth/hubs, so a dashboard started in the wrong directory can
// offer the hubs that do exist. Entries are hints: hub.json in the project
// stays the source of truth, and stale entries are pruned when listed.

// RegistryEnv overrides the registry directory (tests, sandboxes). Test
// binaries without it don't use a registry, so they never touch ~/.tazzyronth.
const RegistryEnv = "TAZZYRONTH_HUBS"

// Running is one registry entry.
type Running struct {
	Dir  string `json:"dir"` // absolute project directory
	PID  int    `json:"pid"`
	Port int    `json:"port"`
}

func registryDir() string {
	if d := os.Getenv(RegistryEnv); d != "" {
		return d
	}
	if testing.Testing() {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tazzyronth", "hubs")
}

func registryPath(dir string) string {
	reg := registryDir()
	if reg == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(dir))))
	return filepath.Join(reg, hex.EncodeToString(sum[:8])+".json")
}

// register records a started hub; failures are ignored (it's only a hint).
func register(r Running) {
	path := registryPath(r.Dir)
	if path == "" || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	b, _ := json.Marshal(r)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, path)
	}
}

// unregister removes the entry if it still belongs to this hub.
func unregister(r Running) {
	path := registryPath(r.Dir)
	if path == "" {
		return
	}
	var cur Running
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &cur) == nil && cur.PID == r.PID && cur.Port == r.Port {
		os.Remove(path)
	}
}

// RunningHubs lists the live hubs on this machine, sorted by directory.
// An entry counts as live when its project's hub.json still names the same
// hub and its port accepts connections; anything else is deleted.
func RunningHubs() []Running {
	reg := registryDir()
	if reg == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(reg, "*.json"))
	var out []Running
	for _, f := range files {
		var r Running
		b, err := os.ReadFile(f)
		if err == nil {
			err = json.Unmarshal(b, &r)
		}
		if err != nil || !alive(r) {
			os.Remove(f)
			continue
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Running) int { return strings.Compare(a.Dir, b.Dir) })
	return out
}

func alive(r Running) bool {
	info, err := ReadInfo(r.Dir)
	if err != nil || info.PID != r.PID || info.Port != r.Port {
		return false
	}
	c, err := net.DialTimeout("tcp", info.Addr(), 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// OtherHubs is RunningHubs without the one for dir.
func OtherHubs(dir string) []Running {
	abs, _ := filepath.Abs(dir)
	return slices.DeleteFunc(RunningHubs(), func(r Running) bool { return strings.EqualFold(r.Dir, abs) })
}
