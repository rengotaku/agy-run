// Package workspace runs a best-effort preflight that warns when agy will write
// to a different directory than the caller's working tree.
//
// In the worktree-first container layout (<repo>/main plus feature worktrees),
// agy ignores its launch cwd and resolves the repo to a trusted workspace root
// (~/code/<repo> -> <repo>/main). A caller sitting in a feature worktree then
// finds agy's edits landing in the sibling main/ instead. This package detects
// that mismatch from agy's own settings.json so agy-run can surface a warning
// rather than silently misdirecting the write.
package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// settings mirrors the fields agy-run reads from agy's settings.json.
type settings struct {
	TrustedWorkspaces []string `json:"trustedWorkspaces"`
}

// Preflight reports a warning when workDir's git root is not itself a trusted
// workspace but a trusted workspace is its sibling worktree (same parent
// container) — the case where agy writes to that sibling instead of workDir.
// Returns "" when there is no mismatch, or when the inputs are unresolvable
// (best-effort: never blocks the run).
func Preflight(workDir, settingsPath string) string {
	root := gitRoot(workDir)
	if root == "" {
		return ""
	}
	realRoot := realpath(root)

	trusted := readTrusted(settingsPath)
	if len(trusted) == 0 {
		return ""
	}

	realTrusted := make([]string, 0, len(trusted))
	for _, t := range trusted {
		if rt := realpath(t); rt != "" {
			realTrusted = append(realTrusted, rt)
		}
	}

	// If the working tree is itself trusted (or nested under a trusted root),
	// agy writes where the caller is — nothing to warn about.
	for _, rt := range realTrusted {
		if realRoot == rt || isUnder(realRoot, rt) {
			return ""
		}
	}

	// The working tree is untrusted. If a trusted workspace is a sibling (same
	// parent container), agy will resolve the repo to that sibling and write
	// there instead.
	parent := filepath.Dir(realRoot)
	for _, rt := range realTrusted {
		if filepath.Dir(rt) == parent {
			return "agy は現在の作業ツリー (" + realRoot +
				") でなく trusted workspace (" + rt +
				") に書き込みます。差分は " + rt +
				" 側に落ちるため、実装後に対象 worktree へ手移送してください。"
		}
	}
	return ""
}

// gitRoot walks up from dir until it finds a ".git" entry (a directory for a
// normal clone, a file for a linked worktree). Returns "" if none is found.
func gitRoot(dir string) string {
	if dir == "" {
		return ""
	}
	d := dir
	for {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// readTrusted returns the trustedWorkspaces entries from agy's settings.json,
// with ~ expanded. Missing/malformed files yield an empty list (no warning).
func readTrusted(settingsPath string) []string {
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil
	}
	var s settings
	if json.Unmarshal(data, &s) != nil {
		return nil
	}
	out := make([]string, 0, len(s.TrustedWorkspaces))
	for _, w := range s.TrustedWorkspaces {
		out = append(out, expandHome(w))
	}
	return out
}

func expandHome(p string) string {
	if p == "~" || len(p) >= 2 && p[:2] == "~/" {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// realpath resolves symlinks; on failure it returns a cleaned path so callers
// can still compare structurally.
func realpath(p string) string {
	if p == "" {
		return ""
	}
	if rp, err := filepath.EvalSymlinks(p); err == nil {
		return rp
	}
	return filepath.Clean(p)
}

// isUnder reports whether path is a strict descendant of base.
func isUnder(path, base string) bool {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}
