package config

import (
	"reflect"
	"testing"
)

func mapEnv(m map[string]string) Env {
	return func(k string) string { return m[k] }
}

func TestParseDefaults(t *testing.T) {
	cfg, help, err := Parse([]string{"hello", "world"}, mapEnv(nil))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if help {
		t.Fatal("did not expect help")
	}
	if cfg.Prompt != "hello world" {
		t.Errorf("prompt = %q, want %q", cfg.Prompt, "hello world")
	}
	if cfg.PrintTimeout != "240s" {
		t.Errorf("print_timeout = %q, want 240s", cfg.PrintTimeout)
	}
	if cfg.LockWait != DefaultLockWait {
		t.Errorf("lock_wait = %d, want %d", cfg.LockWait, DefaultLockWait)
	}
	if cfg.MaxConcurrency != DefaultMaxConcurrency {
		t.Errorf("max_conc = %d, want %d", cfg.MaxConcurrency, DefaultMaxConcurrency)
	}
	if cfg.IdleLimitSec != DefaultIdleLimitSec || cfg.AbsLimitSec != DefaultAbsLimitSec {
		t.Errorf("idle/abs = %d/%d", cfg.IdleLimitSec, cfg.AbsLimitSec)
	}
}

func TestParseFlags(t *testing.T) {
	argv := []string{"-t", "300", "-m", "Gemini 3.5 Flash (High)", "-C", "abc-123", "-w", "60", "-d",
		"-a", "--add-dir", "-a", "/repo", "do", "the", "thing"}
	cfg, _, err := Parse(argv, mapEnv(nil))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cfg.PrintTimeout != "300s" {
		t.Errorf("print_timeout = %q", cfg.PrintTimeout)
	}
	if cfg.Model != "Gemini 3.5 Flash (High)" {
		t.Errorf("model = %q", cfg.Model)
	}
	if cfg.ConvID != "abc-123" || !cfg.Continued() {
		t.Errorf("conv = %q continued=%v", cfg.ConvID, cfg.Continued())
	}
	if cfg.LockWait != 60 || !cfg.Dangerous {
		t.Errorf("lockwait=%d dangerous=%v", cfg.LockWait, cfg.Dangerous)
	}
	if !reflect.DeepEqual(cfg.ExtraArgs, []string{"--add-dir", "/repo"}) {
		t.Errorf("extra = %v", cfg.ExtraArgs)
	}
	if cfg.Prompt != "do the thing" {
		t.Errorf("prompt = %q", cfg.Prompt)
	}
}

func TestParseContinueFlag(t *testing.T) {
	cfg, _, err := Parse([]string{"-c", "again"}, mapEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Continue || !cfg.Continued() {
		t.Errorf("continue=%v", cfg.Continue)
	}
}

func TestParsePrintTimeoutUnitKept(t *testing.T) {
	cfg, _, _ := Parse([]string{"-t", "5m", "x"}, mapEnv(nil))
	if cfg.PrintTimeout != "5m" {
		t.Errorf("print_timeout = %q, want 5m", cfg.PrintTimeout)
	}
}

func TestParseHelp(t *testing.T) {
	_, help, err := Parse([]string{"-h"}, mapEnv(nil))
	if err != nil || !help {
		t.Errorf("help=%v err=%v", help, err)
	}
}

func TestParseEnvKnobs(t *testing.T) {
	env := mapEnv(map[string]string{
		"AGY_RUN_MAX_CONCURRENCY": "5",
		"AGY_RUN_IDLE_LIMIT":      "120",
		"AGY_RUN_ABS_LIMIT":       "1800",
		"AGY_RUN_POLL":            "10",
		"AGY_RUN_TOOL_REPEAT_LIMIT": "5",
	})
	cfg, _, _ := Parse([]string{"x"}, env)
	if cfg.MaxConcurrency != 5 || cfg.IdleLimitSec != 120 || cfg.AbsLimitSec != 1800 || cfg.PollSec != 10 || cfg.ToolRepeatLimit != 5 {
		t.Errorf("env knobs = %+v", cfg)
	}
}

func TestParseMaxConcFloor(t *testing.T) {
	cfg, _, _ := Parse([]string{"x"}, mapEnv(map[string]string{"AGY_RUN_MAX_CONCURRENCY": "0"}))
	if cfg.MaxConcurrency != 1 {
		t.Errorf("max_conc floor = %d, want 1", cfg.MaxConcurrency)
	}
}

func TestParseBadOption(t *testing.T) {
	if _, _, err := Parse([]string{"-Z", "x"}, mapEnv(nil)); err == nil {
		t.Error("expected error for illegal option")
	}
}

func TestParseMissingArg(t *testing.T) {
	if _, _, err := Parse([]string{"-t"}, mapEnv(nil)); err == nil {
		t.Error("expected error for -t without argument")
	}
}

func TestDurationToSec(t *testing.T) {
	cases := map[string]int{"240s": 240, "5m": 300, "90": 90, "bogus": 240}
	for in, want := range cases {
		if got := DurationToSec(in); got != want {
			t.Errorf("DurationToSec(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseWorkspaceShortAndLong(t *testing.T) {
	for _, argv := range [][]string{
		{"-W", "/repo/main", "go"},
		{"--workspace", "/repo/main", "go"},
		{"--workspace=/repo/main", "go"},
	} {
		cfg, _, err := Parse(argv, mapEnv(nil))
		if err != nil {
			t.Fatalf("%v: %v", argv, err)
		}
		if cfg.Workspace != "/repo/main" {
			t.Errorf("%v: workspace = %q", argv, cfg.Workspace)
		}
		if cfg.Prompt != "go" {
			t.Errorf("%v: prompt = %q", argv, cfg.Prompt)
		}
	}
}

func TestParseWaitQuota(t *testing.T) {
	cfg, _, err := Parse([]string{"--wait-quota", "do"}, mapEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.WaitQuota {
		t.Error("wait_quota not set")
	}
	if cfg.MaxQuotaWait != DefaultMaxQuotaWait {
		t.Errorf("max_quota_wait = %d, want %d", cfg.MaxQuotaWait, DefaultMaxQuotaWait)
	}
}

func TestParseLongModel(t *testing.T) {
	cfg, _, err := Parse([]string{"--model", "Gemini 3.5 Flash (High),Gemini 3.1 Pro (High)", "x"}, mapEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Models(), []string{"Gemini 3.5 Flash (High)", "Gemini 3.1 Pro (High)"}) {
		t.Errorf("models = %v", cfg.Models())
	}
}

func TestModelsList(t *testing.T) {
	cases := map[string][]string{
		"":              {""},
		"   ":           {""},
		"A":             {"A"},
		"A,B":           {"A", "B"},
		" A , B ,":      {"A", "B"},
		"Gemini 3.5, X": {"Gemini 3.5", "X"},
	}
	for in, want := range cases {
		got := Config{Model: in}.Models()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Models(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseSandboxFlag(t *testing.T) {
	cfg, _, err := Parse([]string{"--sandbox", "do"}, mapEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Sandbox {
		t.Error("sandbox not set by --sandbox")
	}
}

func TestParseSandboxEnv(t *testing.T) {
	cases := map[string]bool{
		"1":     true,
		"true":  true,
		"TRUE":  true,
		"yes":   true,
		"":      true, // unset (empty) falls back to the default (ON), not env-off
		"0":     false,
		"false": false,
		"FALSE": false,
	}
	for v, want := range cases {
		cfg, _, err := Parse([]string{"do"}, mapEnv(map[string]string{"AGY_RUN_SANDBOX": v}))
		if err != nil {
			t.Fatalf("AGY_RUN_SANDBOX=%q: %v", v, err)
		}
		if cfg.Sandbox != want {
			t.Errorf("AGY_RUN_SANDBOX=%q: sandbox = %v, want %v", v, cfg.Sandbox, want)
		}
	}
}

func TestParseSandboxDefaultOn(t *testing.T) {
	cfg, _, err := Parse([]string{"do"}, mapEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Sandbox {
		t.Error("sandbox should default to true")
	}
}

func TestParseNoSandboxFlag(t *testing.T) {
	cfg, _, err := Parse([]string{"--no-sandbox", "do"}, mapEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sandbox {
		t.Error("sandbox should be false after --no-sandbox")
	}
}

func TestParseSandboxEnvUnsetDefaultsOn(t *testing.T) {
	cfg, _, err := Parse([]string{"do"}, mapEnv(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Sandbox {
		t.Error("sandbox should default to true when AGY_RUN_SANDBOX is unset")
	}
}

// TestParseSandboxPrecedenceFlagOverEnv (issue #15): an explicit flag always
// wins over AGY_RUN_SANDBOX, in either direction.
func TestParseSandboxPrecedenceFlagOverEnv(t *testing.T) {
	cfg, _, err := Parse([]string{"--no-sandbox", "do"}, mapEnv(map[string]string{"AGY_RUN_SANDBOX": "1"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sandbox {
		t.Error("--no-sandbox should win over AGY_RUN_SANDBOX=1")
	}

	cfg, _, err = Parse([]string{"--sandbox", "do"}, mapEnv(map[string]string{"AGY_RUN_SANDBOX": "0"}))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Sandbox {
		t.Error("--sandbox should win over AGY_RUN_SANDBOX=0")
	}
}

func TestParseUnknownLongOption(t *testing.T) {
	if _, _, err := Parse([]string{"--bogus", "x"}, mapEnv(nil)); err == nil {
		t.Error("expected error for unknown long option")
	}
}

func TestGetoptsClusterAndAttached(t *testing.T) {
	opts, rest, err := getopts([]string{"-cd", "-t300", "prompt"}, "t:m:cC:w:da:h")
	if err != nil {
		t.Fatal(err)
	}
	if len(opts) != 3 || opts[0].name != 'c' || opts[1].name != 'd' || opts[2].name != 't' || opts[2].arg != "300" {
		t.Errorf("opts = %+v", opts)
	}
	if !reflect.DeepEqual(rest, []string{"prompt"}) {
		t.Errorf("rest = %v", rest)
	}
}

func TestGetoptsDoubleDash(t *testing.T) {
	_, rest, err := getopts([]string{"-c", "--", "-t", "notaflag"}, "t:cC:")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rest, []string{"-t", "notaflag"}) {
		t.Errorf("rest = %v", rest)
	}
}
