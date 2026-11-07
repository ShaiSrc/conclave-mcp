// Package agent is what the Claude Code integration commands share
// (`hook`, `init`, `agent add`, `messages inbox`): finding the
// repository and its Conclave room, the per-agent state file, and
// reading new messages from other agents.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/hermes/conclave-mcp/internal/cli/client"
)

// ConfigFile is the per-repository config written by `init`.
const ConfigFile = ".conclave.json"

// Config is the content of ConfigFile.
type Config struct {
	Room            string   `json:"room,omitempty"`
	Shared          []string `json:"shared,omitempty"`
	ClaimTTLMinutes int      `json:"claim_ttl_minutes,omitempty"`
}

// DefaultShared are files most agents touch; they are never
// auto-claimed (explicit claims still apply).
var DefaultShared = []string{
	"go.mod", "go.sum", "package.json", "package-lock.json", "pnpm-lock.yaml",
	"yarn.lock", "Cargo.lock", "poetry.lock", "uv.lock",
	"CHANGELOG.md", "README.md", "CLAUDE.md", ".gitignore",
	ConfigFile, ".mcp.json", ".claude/settings.json",
}

// Repo is a git repository and its Conclave config.
type Repo struct {
	Root   string
	Config Config
}

// FindRepo returns the repository containing dir.
func FindRepo(dir string) (Repo, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return Repo{}, fmt.Errorf("%s is not inside a git repository", dir)
	}
	r := Repo{Root: strings.TrimSpace(string(out))}
	if b, err := os.ReadFile(filepath.Join(r.Root, ConfigFile)); err == nil {
		if err := json.Unmarshal(b, &r.Config); err != nil {
			return r, fmt.Errorf("%s: %w", ConfigFile, err)
		}
	}
	return r, nil
}

// RoomName is the room for this repository: Config.Room, else
// "owner/repo" of the origin remote, else the directory name.
func (r Repo) RoomName() string {
	if r.Config.Room != "" {
		return r.Config.Room
	}
	if out, err := exec.Command("git", "-C", r.Root, "remote", "get-url", "origin").Output(); err == nil {
		if name := RoomFromRemote(strings.TrimSpace(string(out))); name != "" {
			return name
		}
	}
	return filepath.Base(r.Root)
}

// RoomFromRemote turns a remote URL into "owner/repo"
// (https://host/owner/repo.git, git@host:owner/repo.git, ssh://...).
func RoomFromRemote(u string) string {
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
		j := strings.Index(u, "/")
		if j < 0 {
			return ""
		}
		u = u[j+1:]
	} else if i := strings.Index(u, ":"); i >= 0 {
		u = u[i+1:]
	}
	parts := strings.Split(u, "/")
	if len(parts) < 2 || parts[len(parts)-2] == "" || parts[len(parts)-1] == "" {
		return ""
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1]
}

// TTL is how long a claim lives without activity.
func (r Repo) TTL() time.Duration {
	if m := r.Config.ClaimTTLMinutes; m > 0 {
		return time.Duration(m) * time.Minute
	}
	return 30 * time.Minute
}

// Rel returns p (absolute, or relative to cwd) relative to the
// repository root with '/' separators; ok is false outside the repo.
func (r Repo) Rel(cwd, p string) (string, bool) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	rel, err := filepath.Rel(resolve(r.Root), resolve(p))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// resolve follows symlinks in the longest existing prefix of p (a
// file about to be written does not exist yet), so /tmp and
// /private/tmp on macOS compare equal.
func resolve(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// IsShared reports whether rel matches a shared pattern, by full path
// or, for patterns without '/', by base name.
func (r Repo) IsShared(rel string) bool {
	patterns := r.Config.Shared
	if patterns == nil {
		patterns = DefaultShared
	}
	for _, pat := range patterns {
		if ok, _ := path.Match(pat, rel); ok {
			return true
		}
		if !strings.Contains(pat, "/") {
			if ok, _ := path.Match(pat, path.Base(rel)); ok {
				return true
			}
		}
	}
	return false
}

// ErrNoRoom: the key is not a member of the repository's room.
var ErrNoRoom = errors.New("not a member of the Conclave room")

// ResolveRoom returns the id of the active room called name among
// the caller's rooms.
func ResolveRoom(ctx context.Context, c *client.Client, name string) (string, error) {
	var resp struct {
		Rooms []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"rooms"`
	}
	if _, err := c.Do(ctx, client.Request{Method: "GET", Path: "/v1/rooms", Out: &resp}); err != nil {
		return "", err
	}
	for _, r := range resp.Rooms {
		if r.Name == name {
			return r.ID, nil
		}
	}
	return "", fmt.Errorf("%w %q (an admin can add this agent: conclave-mcp agent add <label>)", ErrNoRoom, name)
}
