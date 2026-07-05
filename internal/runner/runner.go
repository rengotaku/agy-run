// Package runner orchestrates a single agy-run invocation: concurrency slot,
// post-kill cooldown, process launch with an idle/absolute watchdog, and the
// output contract — exactly one JSON emit.Result on stdout (body plus
// conversation/body_file/artifacts/exit_code/error), with human diagnostics
// (cooldown notice, stall/timeout log tail) on stderr.
package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rengotaku/dotfiles-ubuntu/linux/cmd/agy-run/internal/config"
	"github.com/rengotaku/dotfiles-ubuntu/linux/cmd/agy-run/internal/convlog"
	"github.com/rengotaku/dotfiles-ubuntu/linux/cmd/agy-run/internal/emit"
	"github.com/rengotaku/dotfiles-ubuntu/linux/cmd/agy-run/internal/index"
	"github.com/rengotaku/dotfiles-ubuntu/linux/cmd/agy-run/internal/ratelimit"
	"github.com/rengotaku/dotfiles-ubuntu/linux/cmd/agy-run/internal/slot"
	"github.com/rengotaku/dotfiles-ubuntu/linux/cmd/agy-run/internal/watchdog"
)

// timeNow is overridable in tests; production uses the wall clock.
var timeNow = time.Now

// Options are the runtime dependencies (injected for testability).
type Options struct {
	Stdout io.Writer
	Stderr io.Writer
	Home   string
	Getenv func(string) string
}

// Run executes one invocation and returns the process exit code.
func Run(cfg config.Config, opt Options) int {
	e := emit.New(opt.Stdout, opt.Stderr)
	paths := DefaultPaths(opt.Home, opt.Getenv)

	// Ensure the state dir exists so the slot lock files can be created even on
	// a fresh machine (agy normally creates it, but do not depend on that).
	_ = os.MkdirAll(paths.StateDir, 0o755)

	// Guard 1: cooldown after a prior kill (avoids the exit-144 instant death).
	cooldown(e, paths.KillStamp)

	// Guard 2: bounded-parallel concurrency slot.
	h, ok, err := slot.Acquire(paths.SlotDir, cfg.MaxConcurrency, cfg.LockWait)
	if err != nil {
		e.Emit(emit.Result{ExitCode: config.ExitLockTimeout, Error: &emit.Error{
			Reason: "slot_error", Detail: err.Error(),
		}})
		return config.ExitLockTimeout
	}
	if !ok {
		e.Emit(emit.Result{ExitCode: config.ExitLockTimeout, Error: &emit.Error{
			Reason: "lock_timeout", WaitSec: cfg.LockWait, MaxConcurrency: cfg.MaxConcurrency,
		}})
		return config.ExitLockTimeout
	}
	defer h.Release()

	return execute(cfg, opt, e, paths)
}

// cooldown sleeps out the remainder of the post-kill cooldown window if a kill
// stamp exists and is recent, printing a human advisory to stderr.
func cooldown(e *emit.Emitter, killStamp string) {
	info, err := os.Stat(killStamp)
	if err != nil {
		return
	}
	remain := config.CooldownSec - int(timeNow().Sub(info.ModTime()).Seconds())
	if remain > 0 {
		e.Note(fmt.Sprintf("cooldown: 前回 kill 直後のため %ds 待機します", remain))
		time.Sleep(time.Duration(remain) * time.Second)
	}
}

// execute runs agy under the watchdog and emits the single-object result.
func execute(cfg config.Config, opt Options, e *emit.Emitter, paths Paths) int {
	if err := os.MkdirAll(paths.LogDir, 0o755); err != nil {
		e.Emit(emit.Result{ExitCode: config.ExitUsage, Error: &emit.Error{
			Reason: "logdir_error", Detail: err.Error(),
		}})
		return config.ExitUsage
	}
	start := timeNow()
	logFile := filepath.Join(paths.LogDir,
		fmt.Sprintf("agy-run-%s-%d.log", start.Format("20060102_150405"), os.Getpid()))

	bodyCap, err := os.CreateTemp("", "agy-run-body-*")
	if err != nil {
		e.Emit(emit.Result{ExitCode: config.ExitUsage, Error: &emit.Error{
			Reason: "bodycap_error", Detail: err.Error(),
		}})
		return config.ExitUsage
	}
	bodyPath := bodyCap.Name()
	defer func() { _ = os.Remove(bodyPath) }()

	args := buildArgs(cfg, logFile)

	tsStart := start.Format(time.RFC3339)

	devnull, _ := os.Open(os.DevNull)
	if devnull != nil {
		defer func() { _ = devnull.Close() }()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// agy's stdout is captured (not forwarded live): the raw body is folded into
	// the final JSON result, so nothing but that one object reaches our stdout.
	cmd := commandContext(ctx, paths.AgyBin, append(args, "-p", cfg.Prompt))
	cmd.Stdin = devnull
	cmd.Stdout = bodyCap
	cmd.Stderr = opt.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if serr := cmd.Start(); serr != nil {
		_ = bodyCap.Close()
		e.Emit(emit.Result{ExitCode: 127, Error: &emit.Error{
			Reason: "spawn_failed", Detail: serr.Error(),
		}})
		return 127
	}

	// Watchdog: idle + absolute limits. It kills the process group on a limit.
	w := watchdog.Watcher{
		Start: start,
		Idle:  time.Duration(cfg.IdleLimitSec) * time.Second,
		Abs:   time.Duration(cfg.AbsLimitSec) * time.Second,
		Poll:  time.Duration(cfg.PollSec) * time.Second,
		Paths: heartbeatPaths(logFile, bodyPath, paths.BrainDir, logFile),
	}
	reasonCh := make(chan watchdog.Reason, 1)
	go func() { reasonCh <- w.Run(ctx, func() { killGroup(cmd.Process) }) }()

	waitErr := cmd.Wait()
	cancel()
	reason := <-reasonCh
	_ = bodyCap.Close()

	end := timeNow()
	rc := exitCode(cmd, waitErr, reason)

	convID := convlog.ResolveConvID(logFile)
	body := readBody(bodyPath)

	rec := index.Record{
		TSStart:         tsStart,
		TSEnd:           end.Format(time.RFC3339),
		DurationMS:      end.Sub(start).Milliseconds(),
		ClaudeSessionID: opt.Getenv("CLAUDE_CODE_SESSION_ID"),
		ChildSessionID:  opt.Getenv("CLAUDE_CODE_CHILD_SESSION"),
		ConversationID:  index.ConvPtr(convID),
		Continued:       cfg.Continued(),
		Model:           cfg.Model,
		CWD:             cwd(),
		Prompt:          cfg.Prompt,
		Artifacts:       []string{},
	}

	// Assemble the single result object shared by every exit path.
	r := emit.Result{Body: body}
	if convID != "" {
		r.ConversationID = convID
		r.ContinueCmd = fmt.Sprintf("agy-run -C %s ...", convID)
	}

	// Stall / absolute-timeout branches: log tail to stderr, error in the result.
	if reason == watchdog.Stall || reason == watchdog.AbsTimeout {
		touch(paths.KillStamp)
		printLogTail(e, logFile)
		r.Error = killError(reason, cfg)
		r.ExitCode = rc
		e.Emit(r)
		rec.ExitCode = rc
		index.Append(paths.IndexFile, rec)
		return rc
	}

	// body_file: copy the captured body to a stable per-conversation path.
	if len(body) > 0 && convID != "" {
		if dst, ok := copyBody(paths.LastBodyDir, convID, bodyPath); ok {
			r.BodyFile = dst
		}
	}

	// artifacts: new images written under brain/<conv>/ during this run.
	arts := convlog.NewImageArtifacts(paths.BrainDir, convID, start)
	if len(arts) > 0 {
		rec.Artifacts = arts
		r.Artifacts = arts
	}

	// Rate-limit / empty-output detection when agy returned 0 with no body.
	if rc == config.ExitOK && len(body) == 0 {
		hits := ratelimit.CountHits(logFile)
		if hits > 0 {
			r.Error = &emit.Error{
				Reason: "rate_limited", LogRatelimitHits: hits,
				Hint: "agyの5時間枠/レート上限の可能性。残量を確認し、リセットまで委譲不可（Claude側で実装 or 待機）。",
			}
			rc = config.ExitRateLimited
		} else {
			r.Error = &emit.Error{
				Reason: "empty_output",
				Note:   "本文ゼロ。stderr のログ末尾を確認（rc は変更しない）。",
			}
		}
	}

	r.ExitCode = rc
	e.Emit(r)
	rec.ExitCode = rc
	index.Append(paths.IndexFile, rec)
	return rc
}

// heartbeatPaths returns a provider of the current heartbeat file set. The log
// and body-capture files exist from the start; the conversation transcript is
// added once the conversation id can be resolved from the log.
func heartbeatPaths(logFile, bodyPath, brainDir, logForConv string) func() []string {
	return func() []string {
		ps := []string{logFile, bodyPath}
		if conv := convlog.ResolveConvID(logForConv); conv != "" {
			if tp := convlog.TranscriptPath(brainDir, conv); tp != "" {
				ps = append(ps, tp)
			}
		}
		return ps
	}
}

// killError builds the structured error for a stall (exit 44) or absolute
// timeout (exit 124), pointing the reader at the stderr log tail.
func killError(reason watchdog.Reason, cfg config.Config) *emit.Error {
	if reason == watchdog.Stall {
		return &emit.Error{
			Reason:       "stall",
			IdleLimitSec: cfg.IdleLimitSec,
			Note:         "heartbeat 途絶（agy 応答停止）。stderr にログ末尾を出力。",
		}
	}
	return &emit.Error{
		Reason:       "timeout",
		AbsLimitSec:  cfg.AbsLimitSec,
		PrintTimeout: cfg.PrintTimeout,
		Note:         "絶対上限に到達。stderr にログ末尾を出力。",
	}
}

// printLogTail writes the last few log lines (free text) to stderr for the
// stall/timeout diagnostics.
func printLogTail(e *emit.Emitter, logFile string) {
	e.Plain("\n")
	if tail := tailLines(logFile, 5); tail != "" {
		e.Plain(tail)
	}
}

// readBody returns the captured agy body, or "" if it cannot be read.
func readBody(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// buildArgs assembles the agy argument list (excluding the trailing -p prompt).
func buildArgs(cfg config.Config, logFile string) []string {
	args := []string{"--print-timeout", cfg.PrintTimeout, "--log-file", logFile}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Continue {
		args = append(args, "--continue")
	}
	if cfg.ConvID != "" {
		args = append(args, "--conversation", cfg.ConvID)
	}
	if cfg.Dangerous {
		args = append(args, "--dangerously-skip-permissions")
	}
	args = append(args, cfg.ExtraArgs...)
	return args
}

// exitCode maps the process result and watchdog reason to the contract rc.
func exitCode(cmd *exec.Cmd, waitErr error, reason watchdog.Reason) int {
	switch reason {
	case watchdog.Stall:
		return config.ExitStall
	case watchdog.AbsTimeout:
		return config.ExitAbsTimeout
	}
	if cmd.ProcessState != nil {
		if code := cmd.ProcessState.ExitCode(); code >= 0 {
			return code
		}
	}
	if waitErr == nil {
		return 0
	}
	return 137 // killed by a signal we did not initiate
}

// copyBody copies the captured body to <lastBodyDir>/<conv>.md and returns the
// destination path. ok is false on any I/O error.
func copyBody(lastBodyDir, convID, srcPath string) (string, bool) {
	if err := os.MkdirAll(lastBodyDir, 0o755); err != nil {
		return "", false
	}
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return "", false
	}
	dst := filepath.Join(lastBodyDir, convID+".md")
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", false
	}
	return dst, true
}

func cwd() string {
	d, err := os.Getwd()
	if err != nil {
		return ""
	}
	return d
}

// killGroup sends SIGKILL to the whole process group (Setpgid gave the child its
// own group whose id equals its pid), ensuring agy and its children all die.
func killGroup(p *os.Process) {
	if p == nil {
		return
	}
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
}
