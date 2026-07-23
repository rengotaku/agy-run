// Package config parses agy-run CLI flags (getopts-compatible) and environment
// knobs, and holds the exit-code contract shared across the program.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Exit codes (contract — callers depend on these).
const (
	ExitOK          = 0   // success
	ExitUsage       = 2   // bad flags / missing prompt
	ExitLockTimeout = 4   // could not acquire a concurrency slot within -w
	ExitRateLimited = 42  // empty body + rate-limit signature in log
	ExitStall       = 44  // idle watchdog: heartbeat stalled past idle limit
	ExitAbsTimeout  = 124 // absolute wall-clock limit exceeded
)

// Defaults for the durations and concurrency knobs.
const (
	DefaultPrintTimeout   = "240s"
	DefaultLockWait       = 240   // seconds
	DefaultMaxConcurrency = 3     // AGY_RUN_MAX_CONCURRENCY
	DefaultIdleLimitSec   = 600   // AGY_RUN_IDLE_LIMIT  (10 min)
	DefaultAbsLimitSec    = 3600  // AGY_RUN_ABS_LIMIT   (60 min)
	DefaultPollSec        = 20    // AGY_RUN_POLL
	CooldownSec           = 30    // post-kill cooldown
	DefaultMaxQuotaWait   = 14400 // AGY_RUN_MAX_QUOTA_WAIT (4h cap for --wait-quota)

	// DefaultIndexMaxBytes is the size (bytes) at which internal/index rotates
	// index.jsonl into index.jsonl.1 (kept to 3 generations, oldest dropped).
	// Read directly by internal/index via AGY_RUN_INDEX_MAX_BYTES; 0 disables
	// rotation. Defined here (not threaded through Config) so it follows the
	// same "AGY_RUN_* default + env name" convention as the other knobs above
	// without forcing every Append call site to carry a Config (issue #22).
	DefaultIndexMaxBytes = 10 * 1024 * 1024 // AGY_RUN_INDEX_MAX_BYTES
)

// Config is the fully-resolved invocation.
type Config struct {
	PrintTimeout string   // agy --print-timeout (unit-normalized)
	Model        string   // -m / --model (raw; may be a comma-separated fallback list)
	Continue     bool     // -c
	ConvID       string   // -C
	LockWait     int      // -w (seconds)
	Dangerous    bool     // -d
	ExtraArgs    []string // -a (repeatable)
	Workspace    string   // -W / --workspace (agy cmd.Dir; "" keeps caller cwd)
	WaitQuota    bool     // --wait-quota (parse "Resets in" and wait once, bounded)
	Sandbox      bool     // 既定 ON。--no-sandbox / AGY_RUN_SANDBOX=0 で無効化（bwrap isolation + net logging）
	Prompt       string   // joined trailing args

	MaxConcurrency int // AGY_RUN_MAX_CONCURRENCY (>=1)
	IdleLimitSec   int // AGY_RUN_IDLE_LIMIT
	AbsLimitSec    int // AGY_RUN_ABS_LIMIT
	PollSec        int // AGY_RUN_POLL
	MaxQuotaWait   int // AGY_RUN_MAX_QUOTA_WAIT (seconds; --wait-quota upper bound)
}

// Env abstracts environment lookup for testability.
type Env func(string) string

// Parse resolves flags (argv without program name) and environment into a
// Config. On -h it returns wantHelp=true. A nil env falls back to os.Getenv.
func Parse(argv []string, env Env) (cfg Config, wantHelp bool, err error) {
	if env == nil {
		env = os.Getenv
	}
	cfg = Config{
		PrintTimeout: DefaultPrintTimeout,
		LockWait:     DefaultLockWait,
		Sandbox:      true,
	}

	opts, rest, err := getopts(argv, "t:m:cC:w:W:da:h", longOpts)
	if err != nil {
		return cfg, false, err
	}
	var sandboxFlagSet bool
	for _, o := range opts {
		switch o.name {
		case 't':
			cfg.PrintTimeout = o.arg
		case 'm':
			cfg.Model = o.arg
		case 'c':
			cfg.Continue = true
		case 'C':
			cfg.ConvID = o.arg
		case 'w':
			w, e := strconv.Atoi(o.arg)
			if e != nil {
				return cfg, false, fmt.Errorf("invalid -w value: %q", o.arg)
			}
			cfg.LockWait = w
		case 'd':
			cfg.Dangerous = true
		case 'a':
			cfg.ExtraArgs = append(cfg.ExtraArgs, o.arg)
		case 'W':
			cfg.Workspace = o.arg
		case 'Q': // internal short byte for --wait-quota
			cfg.WaitQuota = true
		case 'S': // internal short byte for --sandbox
			cfg.Sandbox = true
			sandboxFlagSet = true
		case 'N': // internal short byte for --no-sandbox
			cfg.Sandbox = false
			sandboxFlagSet = true
		case 'h':
			return cfg, true, nil
		}
	}

	cfg.Prompt = strings.Join(rest, " ")

	// --print-timeout requires a unit; a bare integer is normalized to seconds
	// (agy rejects a unitless "240" as an invalid value).
	cfg.PrintTimeout = normalizeDuration(cfg.PrintTimeout)

	cfg.MaxConcurrency = envInt(env, "AGY_RUN_MAX_CONCURRENCY", DefaultMaxConcurrency)
	if cfg.MaxConcurrency < 1 {
		cfg.MaxConcurrency = 1
	}
	cfg.IdleLimitSec = envInt(env, "AGY_RUN_IDLE_LIMIT", DefaultIdleLimitSec)
	cfg.AbsLimitSec = envInt(env, "AGY_RUN_ABS_LIMIT", DefaultAbsLimitSec)
	cfg.PollSec = envInt(env, "AGY_RUN_POLL", DefaultPollSec)
	if cfg.PollSec < 1 {
		cfg.PollSec = 1
	}
	cfg.MaxQuotaWait = envInt(env, "AGY_RUN_MAX_QUOTA_WAIT", DefaultMaxQuotaWait)

	// Sandbox precedence: flag (--sandbox/--no-sandbox) > AGY_RUN_SANDBOX env >
	// default (ON). AGY_RUN_SANDBOX=0/false disables, 1/true (or any other
	// non-empty value) enables; it is only consulted when no flag was given.
	if !sandboxFlagSet {
		if v := strings.TrimSpace(env("AGY_RUN_SANDBOX")); v != "" {
			cfg.Sandbox = envBool(env, "AGY_RUN_SANDBOX")
		}
	}

	return cfg, false, nil
}

// longOpts folds agy-run's long flags onto the internal short bytes Parse
// dispatches on. --wait-quota, --sandbox and --no-sandbox have no short form,
// so they use the private bytes 'Q', 'S' and 'N'.
var longOpts = map[string]longOpt{
	"workspace":  {short: 'W', takesArg: true},
	"model":      {short: 'm', takesArg: true},
	"wait-quota": {short: 'Q', takesArg: false},
	"sandbox":    {short: 'S', takesArg: false},
	"no-sandbox": {short: 'N', takesArg: false},
}

// Continued reports whether this call continues an existing conversation.
func (c Config) Continued() bool { return c.Continue || c.ConvID != "" }

// Models splits -m on commas into an ordered fallback list (each trimmed,
// empties dropped). With no -m it returns a single empty element so callers can
// run agy on its default model with one uniform loop iteration.
func (c Config) Models() []string {
	if strings.TrimSpace(c.Model) == "" {
		return []string{""}
	}
	var out []string
	for _, m := range strings.Split(c.Model, ",") {
		if s := strings.TrimSpace(m); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

// normalizeDuration appends "s" to a bare integer duration ("240" -> "240s").
func normalizeDuration(d string) string {
	if d == "" {
		return DefaultPrintTimeout
	}
	if _, err := strconv.Atoi(d); err == nil {
		return d + "s"
	}
	return d
}

// DurationToSec converts a unit-suffixed duration ("240s", "5m", "240") to
// seconds, mirroring the bash duration_to_sec (fallback 240).
func DurationToSec(d string) int {
	switch {
	case strings.HasSuffix(d, "m"):
		if n, err := strconv.Atoi(strings.TrimSuffix(d, "m")); err == nil {
			return n * 60
		}
	case strings.HasSuffix(d, "s"):
		if n, err := strconv.Atoi(strings.TrimSuffix(d, "s")); err == nil {
			return n
		}
	default:
		if n, err := strconv.Atoi(d); err == nil {
			return n
		}
	}
	return 240
}

func envInt(env Env, key string, def int) int {
	v := env(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// envBool reports a boolean env knob: unset/"0"/"false" (case-insensitive)
// are false, anything else (including "1", "true", "yes") is true.
func envBool(env Env, key string) bool {
	v := strings.TrimSpace(env(key))
	if v == "" {
		return false
	}
	return !strings.EqualFold(v, "0") && !strings.EqualFold(v, "false")
}
