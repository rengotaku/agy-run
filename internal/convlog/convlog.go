// Package convlog resolves the conversation id from the agy log and detects
// image artifacts written under brain/<conv_id>/ during the run.
package convlog

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	// "conversation=<uuid>"
	convEqRe = regexp.MustCompile(`conversation=([0-9a-f-]{36})`)
	// "Created conversation <uuid>"
	convCreatedRe = regexp.MustCompile(`Created conversation ([0-9a-f-]{36})`)
)

// imageExts are the artifact image extensions agy writes.
var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true,
}

// ResolveConvID extracts the conversation id from the log file. It prefers the
// last "conversation=<uuid>" and falls back to the last
// "Created conversation <uuid>" (matching the bash resolution order). Returns
// "" if none is found or the log is unreadable.
func ResolveConvID(logPath string) string {
	f, err := os.Open(logPath)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	return resolveConvID(f)
}

func resolveConvID(r io.Reader) string {
	var lastEq, lastCreated string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if m := convEqRe.FindSubmatch(line); m != nil {
			lastEq = string(m[1])
		}
		if m := convCreatedRe.FindSubmatch(line); m != nil {
			lastCreated = string(m[1])
		}
	}
	if lastEq != "" {
		return lastEq
	}
	return lastCreated
}

// TranscriptPath returns the transcript.jsonl path for a conversation under the
// gemini state dir (brain/<conv>/.system_generated/logs/transcript.jsonl).
// Returns "" for an empty conv id.
func TranscriptPath(brainDir, convID string) string {
	if convID == "" {
		return ""
	}
	return filepath.Join(brainDir, convID, ".system_generated", "logs", "transcript.jsonl")
}

// NewImageArtifacts lists image files under brain/<conv_id>/ whose modification
// time is at or after 'since' (the pre-run marker), so --continue reruns do not
// re-report pre-existing images. Paths are absolute and sorted for determinism.
func NewImageArtifacts(brainDir, convID string, since time.Time) []string {
	if convID == "" {
		return nil
	}
	root := filepath.Join(brainDir, convID)
	var out []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !imageExts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if info.ModTime().Before(since) {
			return nil
		}
		out = append(out, path)
		return nil
	})
	sort.Strings(out)
	return out
}
