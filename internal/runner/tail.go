package runner

import (
	"os"
	"strings"
)

// tailLines returns the last n lines of the file at path as a single string
// (newline-joined, trailing newline preserved). Returns "" if unreadable.
func tailLines(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text := string(data)
	hadTrailing := strings.HasSuffix(text, "\n")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := strings.Join(lines, "\n")
	if hadTrailing {
		out += "\n"
	}
	return out
}
