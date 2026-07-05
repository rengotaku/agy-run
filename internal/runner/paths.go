package runner

import (
	"os"
	"path/filepath"
)

// Paths holds every filesystem location agy-run touches. Resolved once from the
// environment so tests can point them at temp dirs.
type Paths struct {
	AgyBin      string // agy binary
	StateDir    string // ~/.gemini/antigravity-cli
	BrainDir    string // <state>/brain (image artifacts + transcripts)
	LogDir      string // <state>/log
	LastBodyDir string // <state>/last-body
	IndexFile   string // index.jsonl
	KillStamp   string // .agy-run.last-kill
	SlotDir     string // dir holding .agy-run.slot.N lock files
	Settings    string // <state>/settings.json (trustedWorkspaces for preflight)
}

// DefaultPaths builds the standard layout under home, honoring XDG_DATA_HOME
// for the index file (matching the bash defaults).
func DefaultPaths(home string, getenv func(string) string) Paths {
	state := filepath.Join(home, ".gemini", "antigravity-cli")
	dataHome := getenv("XDG_DATA_HOME")
	if dataHome == "" {
		dataHome = filepath.Join(home, ".local", "share")
	}
	return Paths{
		AgyBin:      filepath.Join(home, ".local", "bin", "agy"),
		StateDir:    state,
		BrainDir:    filepath.Join(state, "brain"),
		LogDir:      filepath.Join(state, "log"),
		LastBodyDir: filepath.Join(state, "last-body"),
		IndexFile:   filepath.Join(dataHome, "agy-run", "index.jsonl"),
		KillStamp:   filepath.Join(state, ".agy-run.last-kill"),
		SlotDir:     state,
		Settings:    filepath.Join(state, "settings.json"),
	}
}

// touch creates/updates the mtime of a file (used for the post-kill cooldown
// stamp). Errors are ignored — the stamp is advisory.
func touch(path string) {
	now := timeNow()
	if err := os.Chtimes(path, now, now); err != nil {
		if f, e := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600); e == nil {
			_ = f.Close()
		}
	}
}
