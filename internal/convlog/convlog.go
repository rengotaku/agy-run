// Package convlog resolves the conversation id from the agy log and detects
// image artifacts written under brain/<conv_id>/ during the run.
package convlog

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rengotaku/agy-run/internal/emit"
)

var (
	// "conversation=<uuid>"
	convEqRe = regexp.MustCompile(`conversation=([0-9a-f-]{36})`)
	// "Created conversation <uuid>"
	convCreatedRe = regexp.MustCompile(`Created conversation ([0-9a-f-]{36})`)
	// "The user changed setting `Model Selection` from ... to <ModelName>."
	modelSelectionRe = regexp.MustCompile(`The user changed setting ` + "`" + `Model Selection` + "`" + ` from .*? to (.*?)(?:\. |\.$|\."|\.\n|\.\r|"\n|"\r|"$|\n|\r|$)`)
)

// UnresolvedModel is recorded in index.jsonl when effective model resolution fails or yields no value.
const UnresolvedModel = "(unresolved)"

// NormalizeModelName maps a display name to its kebab-case ID using modelMap.
// If modelMap is nil or does not contain displayName, displayName is returned as-is.
func NormalizeModelName(displayName string, modelMap map[string]string) string {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return ""
	}
	if modelMap != nil {
		if id, ok := modelMap[displayName]; ok && id != "" {
			return id
		}
	}
	return displayName
}

// ExtractModelFromTranscript scans transcript.jsonl and returns the last Model Selection display name.
// Returns "" if transcriptPath is empty, unreadable, or contains no Model Selection.
func ExtractModelFromTranscript(transcriptPath string) string {
	if transcriptPath == "" {
		return ""
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	var lastModel string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if m := modelSelectionRe.FindSubmatch(line); m != nil {
			name := strings.TrimSpace(string(m[1]))
			name = strings.TrimSuffix(name, ".")
			if name != "" {
				lastModel = name
			}
		}
	}
	return lastModel
}

type settingsFile struct {
	Model string `json:"model"`
}

// ExtractModelFromSettings reads settings.json and returns the "model" string.
// Returns "" if settingsPath is empty, unreadable, invalid JSON, or lacks model.
func ExtractModelFromSettings(settingsPath string) string {
	if settingsPath == "" {
		return ""
	}
	b, err := os.ReadFile(settingsPath)
	if err != nil {
		return ""
	}
	var sf settingsFile
	if err := json.Unmarshal(b, &sf); err != nil {
		return ""
	}
	return strings.TrimSpace(sf.Model)
}

// ResolveEffectiveModel resolves the effective model following the priority order:
// 1. flagModel (-m)
// 2. Model Selection in transcriptPath (normalized to ID if possible)
// 3. model in settingsPath (normalized to ID if possible)
// 4. UnresolvedModel
func ResolveEffectiveModel(flagModel, transcriptPath, settingsPath string, modelMap map[string]string) (result string) {
	defer func() {
		if r := recover(); r != nil {
			result = UnresolvedModel
		}
	}()

	flagModel = strings.TrimSpace(flagModel)
	if flagModel != "" {
		return flagModel
	}

	if transcriptPath != "" {
		if m := ExtractModelFromTranscript(transcriptPath); m != "" {
			return NormalizeModelName(m, modelMap)
		}
	}

	if settingsPath != "" {
		if m := ExtractModelFromSettings(settingsPath); m != "" {
			return NormalizeModelName(m, modelMap)
		}
	}

	return UnresolvedModel
}



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

// ArtifactTools holds the set of tool names that generate artifacts.
var ArtifactTools = map[string]bool{
	"generate_image": true,
}

type transcriptEntry struct {
	CreatedAt string `json:"created_at"`
	ToolCalls []struct {
		Name string `json:"name"`
	} `json:"tool_calls"`
}

// CountArtifactToolCalls scans a transcript file and counts calls to artifact-generating tools
// occurring at or after 'since'. If 'since' is zero, all calls are counted.
// Returns 0 if transcriptPath is empty, unreadable, or contains no matching artifact tool calls.
func CountArtifactToolCalls(transcriptPath string, since time.Time) int {
	if transcriptPath == "" {
		return 0
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()

	if !since.IsZero() {
		since = since.Truncate(time.Second)
	}

	count := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		var entry transcriptEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if !since.IsZero() {
			if entry.CreatedAt == "" {
				continue
			}
			t, err := time.Parse(time.RFC3339Nano, entry.CreatedAt)
			if err != nil || t.Before(since) {
				continue
			}
		}
		for _, tc := range entry.ToolCalls {
			if ArtifactTools[tc.Name] {
				count++
			}
		}
	}
	return count
}

// CheckArtifactMissing evaluates if artifact-generating tools were called but no artifacts were produced.
// Returns an *emit.Error with reason "artifact_missing" if toolCalls > 0 and len(artifacts) == 0, nil otherwise.
func CheckArtifactMissing(toolCalls int, artifacts []string) *emit.Error {
	if toolCalls > 0 && len(artifacts) == 0 {
		return &emit.Error{
			Reason: "artifact_missing",
			Hint:   "成果物生成ツールが実行されましたが成果物が生成されませんでした。",
		}
	}
	return nil
}


