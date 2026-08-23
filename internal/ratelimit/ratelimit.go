// Package ratelimit detects the "empty body + rate-limit signature" case:
// agy can give up on a 429 (5-hour quota / model overload) yet still exit 0
// with zero body, which would otherwise read as a silent success.
package ratelimit

import (
	"bufio"
	"io"
	"os"
	"regexp"
	"strconv"
	"time"
)

// sigRe matches the rate-limit / overload signatures in the agy log
// (case-insensitive), mirroring the bash grep pattern
// '429|overloaded|rate.?limit|resource_exhausted|quota'.
var sigRe = regexp.MustCompile(`(?i)\b429\b|overloaded|rate.?limit|resource_exhausted|\bquota\b`)

// resetRe captures the agy quota-reset hint, e.g. "Resets in 3h0m10s" or
// "Resets in 45m 30s". Hours/minutes/seconds are each optional.
var resetRe = regexp.MustCompile(`(?i)resets? in\s+(?:(\d+)h)?\s*(?:(\d+)m)?\s*(?:(\d+)s)?`)

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

// ResetWait scans the log for the last "Resets in Xh Ym Zs" hint and returns
// the parsed wait duration. ok is false when no reset hint is present (or the
// log is unreadable), so the caller does not wait blindly.
func ResetWait(logPath string) (d time.Duration, ok bool) {
	f, err := os.Open(logPath)
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()
	return resetWait(f)
}

func resetWait(r io.Reader) (time.Duration, bool) {
	var last []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if m := resetRe.FindStringSubmatch(sc.Text()); m != nil {
			last = m
		}
	}
	if last == nil {
		return 0, false
	}
	h := atoiOr0(last[1])
	m := atoiOr0(last[2])
	s := atoiOr0(last[3])
	if h == 0 && m == 0 && s == 0 {
		return 0, false
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second, true
}

func atoiOr0(s string) int {
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
