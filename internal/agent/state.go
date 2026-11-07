package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// State is the per-key, per-scope state file: inbox cursor, own key
// id, the resolved room and the last claims heartbeat.
type State struct {
	Since     string `json:"since"`
	SelfID    int64  `json:"self_id"`
	RoomID    string `json:"room_id,omitempty"`
	Heartbeat int64  `json:"heartbeat,omitempty"` // unix seconds
}

// StatePath is the state file of a key (by its public prefix) for a
// scope (a room id, or "repo-<hash>" for the hook). dir "" means
// <user cache dir>/conclave-mcp.
func StatePath(dir, token, scope string) string {
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			base = os.TempDir()
		}
		dir = filepath.Join(base, "conclave-mcp")
	}
	prefix := token
	if len(token) >= 11 {
		prefix = token[3:11] // the public key prefix shown by `keys list`
	}
	return filepath.Join(dir, "inbox-"+prefix+"-"+scope+".json")
}

func LoadState(path string) State {
	var st State
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

// SaveState writes via temp file + rename: parallel hooks must never
// read a half-written file (that would look like a first run and
// replay old messages).
func SaveState(path string, st State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".inbox-*")
	if err != nil {
		return err
	}
	b, _ := json.Marshal(st)
	_, werr := tmp.Write(b)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return werr
	}
	return os.Rename(tmp.Name(), path)
}
