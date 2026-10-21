// Package printtimeout detects when agy stopped due to --print-timeout while
// running in non-interactive print mode.
package printtimeout

import (
	"bufio"
	"io"
	"os"
	"regexp"
)

// timeoutRe matches the agy glog status transition indicating that print mode
// timed out. Including the source-location tag prevents false positives from
// prompt contents or search results.
var timeoutRe = regexp.MustCompile(`poll\.go:\d+\] Print mode: print timeout after \S+ with turn in progress`)

// Detect scans the log file for a print timeout log line.
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
	br := bufio.NewReader(r)
	for {
		l, isPrefix, err := br.ReadLine()
		if err != nil && err != io.EOF {
			return "", false
		}
		if isPrefix {
			for isPrefix && err == nil {
				_, isPrefix, err = br.ReadLine()
			}
			if err != nil {
				return "", false
			}
			continue
		}
		if len(l) > 0 && timeoutRe.Match(l) {
			return string(l), true
		}
		if err == io.EOF {
			break
		}
	}
	return "", false
}
