// Package runcmd extracts agy's RUN_COMMAND (build/test) step outcomes from the
// agy log so agy-run can flag when agy's self-reported "tests are green" is
// contradicted by a failed command step (e.g. a cwd misresolution that stopped
// go build / go test from ever running).
//
// This is best-effort by design: it keys off the documented failure line
// ("error executing cascade step: CORTEX_STEP_TYPE_RUN_COMMAND: ...") and must
// never crash or block the run when the log format shifts — an unrecognized log
// simply yields zero failures.
package runcmd

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// stepMarker identifies an agy RUN_COMMAND cascade step in the log.
const stepMarker = "CORTEX_STEP_TYPE_RUN_COMMAND"

// failureCues are substrings that mark a RUN_COMMAND step line as failed.
var failureCues = []string{
	"error executing cascade step",
	"no such directory",
	"no such file or directory",
	"exit status",
	"command failed",
}

// Failure is one detected RUN_COMMAND failure (the trimmed log line, capped).
type Failure struct {
	Detail string
}

// Scan returns the RUN_COMMAND failures found in the log at path. A missing or
// unreadable log yields no failures (best-effort; never an error).
func Scan(path string) []Failure {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	return scan(f)
}

func scan(r io.Reader) []Failure {
	var out []Failure
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, stepMarker) {
			continue
		}
		lower := strings.ToLower(line)
		for _, cue := range failureCues {
			if strings.Contains(lower, cue) {
				out = append(out, Failure{Detail: trim(line)})
				break
			}
		}
	}
	return out
}

// trim collapses surrounding whitespace and caps the detail length so a runaway
// log line cannot bloat the result JSON.
func trim(s string) string {
	s = strings.TrimSpace(s)
	const max = 300
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
