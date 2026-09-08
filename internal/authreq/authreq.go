// Package authreq detects when agy triggered an interactive OAuth flow while
// running in non-interactive print mode.
package authreq

import (
	"bufio"
	"io"
	"os"
	"regexp"
)

// authReqRe matches the agy glog status transition indicating that print mode
// triggered interactive OAuth. Including the source-location tag prevents false
// positives from prompt contents or search results.
var authReqRe = regexp.MustCompile(`printmode\.go:\d+\] Print mode: triggering interactive OAuth$`)

// Detect scans the log file for an interactive OAuth transition line.
// Returns the matching line and true if found; returns ("", false) if not
// found or if the file cannot be opened or read.
func Detect(logPath string) (line string, ok bool) {
	f, err := os.Open(logPath)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	return detect(f)
}

func detect(r io.Reader) (line string, ok bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		text := sc.Text()
		if authReqRe.MatchString(text) {
			return text, true
		}
	}
	return "", false
}
