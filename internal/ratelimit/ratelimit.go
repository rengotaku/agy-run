// Package ratelimit detects the "empty body + rate-limit signature" case:
// agy can give up on a 429 (5-hour quota / model overload) yet still exit 0
// with zero body, which would otherwise read as a silent success.
package ratelimit

import (
	"bufio"
	"io"
	"os"
	"regexp"
)

// sigRe matches the rate-limit / overload signatures in the agy log
// (case-insensitive), mirroring the bash grep pattern
// '429|overloaded|rate.?limit|resource_exhausted|quota'.
var sigRe = regexp.MustCompile(`(?i)429|overloaded|rate.?limit|resource_exhausted|quota`)

// CountHits returns the number of log lines that match a rate-limit signature.
// A missing/unreadable log yields 0 (no false positive).
func CountHits(logPath string) int {
	f, err := os.Open(logPath)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	return countHits(f)
}

func countHits(r io.Reader) int {
	n := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if sigRe.Match(sc.Bytes()) {
			n++
		}
	}
	return n
}
