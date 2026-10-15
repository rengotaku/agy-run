// Package runner orchestrates a single agy-run invocation: concurrency slot,
// post-kill cooldown, process launch with an idle/absolute watchdog, and the
// output contract — exactly one JSON emit.Result on stdout (body plus
// conversation/body_file/artifacts/exit_code/error), with human diagnostics
// (cooldown notice, stall/timeout log tail) on stderr.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rengotaku/agy-run/internal/authreq"
	"github.com/rengotaku/agy-run/internal/config"
	"github.com/rengotaku/agy-run/internal/convlog"
	"github.com/rengotaku/agy-run/internal/emit"
	"github.com/rengotaku/agy-run/internal/index"
	"github.com/rengotaku/agy-run/internal/modelalias"
	"github.com/rengotaku/agy-run/internal/ratelimit"
	"github.com/rengotaku/agy-run/internal/runcmd"
	"github.com/rengotaku/agy-run/internal/sandbox"
	"github.com/rengotaku/agy-run/internal/slot"
	"github.com/rengotaku/agy-run/internal/watchdog"
	"github.com/rengotaku/agy-run/internal/workspace"
)

const authRequiredHint = "agy が非対話実行中に OAuth ログインを要求した。keyring (D-Bus) に届いていない可能性が高い。Claude Code の Bash sandbox 内で動いていないか確認する。sandbox.excludedCommands の `agy-run *` は単独のコマンドにしか一致せず、`cd X && agy-run ...` やパイプでつなぐと効かない。単独で呼んでも、環境変数を前に置く形や、プロンプトにバッククォートか `$(` を含む形は一致しない。cd の代わりに `-W <dir>` を使って agy-run を単独で呼ぶか、Bash ツールに dangerouslyDisableSandbox を付ける。手元でログインし直すなら引数なしで agy を起動する。"

// timeNow and sleepFor are overridable in tests; production uses the wall clock.
var (
	timeNow   = time.Now
	sleepFor  = time.Sleep
	afterWait func(context.Context)
)

// Options are the runtime dependencies (injected for testability).
type Options struct {
	Stdout  io.Writer
	Stderr  io.Writer
	Home    string
	Getenv  func(string) string
	Signals <-chan os.Signal
}

// Run executes one invocation and returns the process exit code.
func Run(cfg config.Config, opt Options) int {
	e := emit.New(opt.Stdout, opt.Stderr)
	paths := DefaultPaths(opt.Home, opt.Getenv)

	// Detach FIRST, before any real work: move agy-run into its own process
	// group so a launcher-group teardown (Claude's background Bash killed with
	// `kill -- -<pgid>`) no longer cuts the delegation down mid-flight. agy-run
	// still reaps agy on a signal to itself (watchdog / explicit kill), so the
	// #7/#8 orphan fix stays intact (issue #9). Silent: the clean-success stderr
	// contract stays empty.
	detachFromLauncher()

	// Ensure the state dir exists so the slot lock files can be created even on
	// a fresh machine (agy normally creates it, but do not depend on that).
	_ = os.MkdirAll(paths.StateDir, 0o755)

	// Guard 0: validate or resolve -m candidates.
	// When -m starts with '@', resolve the alias against models.json, drop
	// candidates absent from `agy models`, and pass the remaining chain to
	// execution. Direct model names (@ absent) are strictly validated as before.
	var resolvedModels []string
	var droppedModels []string

	if cfg.Model != "" {
		aliasName, isRef, err := modelalias.ParseRef(cfg.Model)
		if err != nil {
			e.Emit(emit.Result{ExitCode: config.ExitUsage, Error: &emit.Error{
				Reason: "invalid_model_alias",
				Detail: cfg.Model,
			}})
			return config.ExitUsage
		}

		if isRef {
			modelsPath := modelalias.DefaultPath(opt.Getenv, opt.Home)
			chains, err := modelalias.Load(modelsPath)
			if err != nil {
				e.Emit(emit.Result{ExitCode: config.ExitUsage, Error: &emit.Error{
					Reason: "models_file_error",
					Detail: err.Error(),
				}})
				return config.ExitUsage
			}

			chain, err := chains.Resolve(aliasName)
			if err != nil {
				var unknownErr *modelalias.UnknownAliasError
				hint := ""
				if errors.As(err, &unknownErr) {
					hint = strings.Join(unknownErr.Known, ", ")
				}
				e.Emit(emit.Result{ExitCode: config.ExitUsage, Error: &emit.Error{
					Reason: "unknown_model_alias",
					Detail: aliasName,
					Hint:   hint,
				}})
				return config.ExitUsage
			}

			if available, ok := listModels(paths.AgyBin); ok {
				kept, dropped := modelalias.FilterAvailable(chain, available)
				droppedModels = dropped
				if len(kept) == 0 {
					e.Emit(emit.Result{
						ExitCode:      config.ExitUsage,
						DroppedModels: dropped,
						Error: &emit.Error{
							Reason:          "no_available_model",
							Detail:          "@" + aliasName,
							AvailableModels: available,
						},
					})
					return config.ExitUsage
				}
				resolvedModels = kept
			} else {
				resolvedModels = chain
			}
		} else {
			if models, ok := listModels(paths.AgyBin); ok {
				for _, m := range cfg.Models() {
					if m != "" && !contains(models, m) {
						e.Emit(emit.Result{ExitCode: config.ExitUsage, Error: &emit.Error{
							Reason:          "unknown_model",
							Detail:          m,
							AvailableModels: models,
							Hint:            `-m は agy models の名称と完全一致が必要（kebab-case ID。例: "gemini-3.6-flash-high"）。カンマ区切りでフォールバック候補を並べられます。`,
						}})
						return config.ExitUsage
					}
				}
			}
			resolvedModels = cfg.Models()
		}
	} else {
		resolvedModels = cfg.Models()
	}

	// Guard 1: cooldown after a prior kill (avoids the exit-144 instant death).
	cooldown(e, paths.KillStamp)

	// Guard 2: bounded-parallel concurrency slot.
	h, ok, err := slot.Acquire(paths.SlotDir, cfg.MaxConcurrency, cfg.LockWait)
	if err != nil {
		e.Emit(emit.Result{
			ExitCode:      config.ExitLockTimeout,
			DroppedModels: droppedModels,
			Error: &emit.Error{
				Reason: "slot_error", Detail: err.Error(),
			},
		})
		return config.ExitLockTimeout
	}
	if !ok {
		e.Emit(emit.Result{
			ExitCode:      config.ExitLockTimeout,
			DroppedModels: droppedModels,
			Error: &emit.Error{
				Reason: "lock_timeout", WaitSec: cfg.LockWait, MaxConcurrency: cfg.MaxConcurrency,
			},
		})
		return config.ExitLockTimeout
	}
	defer h.Release()

	// Signal-aware context: a received SIGINT/SIGTERM/SIGHUP cancels the run so
	// the agy child (and its whole process group) is torn down instead of being
	// orphaned when agy-run itself is terminated by its launcher — orphaned agy
	// processes otherwise linger for days holding inotify watches (issue #7).
	// We retain the caught signal so attempt() can distinguish our own signal
	// cancellation from external kills (issue #53).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctx, tracker := contextWithSignal(ctx)

	sigCh := opt.Signals
	var stopSignal func()
	if sigCh == nil {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		sigCh = ch
		stopSignal = func() { signal.Stop(ch) }
	}
	if stopSignal != nil {
		defer stopSignal()
	}

	// テストの決定性のため、Run はシグナル監視を開始する時点で Options.Signals に既に入っているシグナルを同期的に取り込む
	select {
	case sig, ok := <-sigCh:
		if ok && sig != nil {
			tracker.set(sig)
			cancel()
		}
	default:
	}

	sigDone := make(chan struct{})
	defer close(sigDone)
	go func() {
		select {
		case sig, ok := <-sigCh:
			if ok && sig != nil {
				tracker.set(sig)
				cancel()
			}
		case <-sigDone:
		}
	}()

	return execute(ctx, cfg, opt, e, paths, resolvedModels, droppedModels)
}

type sigKey struct{}

type signalTracker struct {
	mu  sync.Mutex
	sig os.Signal
}

func (st *signalTracker) get() os.Signal {
	if st == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.sig
}

func (st *signalTracker) set(sig os.Signal) {
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sig == nil {
		st.sig = sig
	}
}

func contextWithSignal(ctx context.Context) (context.Context, *signalTracker) {
	st := &signalTracker{}
	return context.WithValue(ctx, sigKey{}, st), st
}

func signalFromContext(ctx context.Context) os.Signal {
	if st, ok := ctx.Value(sigKey{}).(*signalTracker); ok {
		return st.get()
	}
	return nil
}

func signalName(sig os.Signal) string {
	switch sig {
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGHUP:
		return "SIGHUP"
	default:
		if sysSig, ok := sig.(syscall.Signal); ok {
			return fmt.Sprintf("signal %d", int(sysSig))
		}
		return sig.String()
	}
}

// waitQuotaSleep pauses for duration d or until ctx is cancelled.
// In tests, sleepFor may be overridden to record or fake the wait.
func waitQuotaSleep(ctx context.Context, d time.Duration) error {
	if reflect.ValueOf(sleepFor).Pointer() != reflect.ValueOf(time.Sleep).Pointer() {
		sleepFor(d)
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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

// execute drives the model-fallback / quota-wait loop around attempt() and
// emits the single result object exactly once. All physical agy runs happen
// inside the already-acquired concurrency slot; only rate_limited triggers a
// retry (fall to the next -m candidate, then optionally wait out the quota).
func execute(ctx context.Context, cfg config.Config, opt Options, e *emit.Emitter, paths Paths, models, droppedModels []string) int {
	preflight := workspace.Preflight(effectiveWorkDir(cfg), paths.Settings)

	waited := false
	for i := 0; ; {
		model := models[i]
		attemptStart := timeNow()
		r, rc, logFile := attempt(ctx, cfg, model, opt, e, paths)
		if len(droppedModels) > 0 {
			r.DroppedModels = droppedModels
		}

		appendInterruptedIndex := func(sigExit int) {
			end := timeNow()
			attemptModel := model
			if attemptModel == "" && r.Model != "" {
				attemptModel = r.Model
			}
			rec := index.Record{
				TSStart:         attemptStart.Format(time.RFC3339),
				TSEnd:           end.Format(time.RFC3339),
				DurationMS:      end.Sub(attemptStart).Milliseconds(),
				ClaudeSessionID: opt.Getenv("CLAUDE_CODE_SESSION_ID"),
				ChildSessionID:  opt.Getenv("CLAUDE_CODE_CHILD_SESSION"),
				ConversationID:  index.ConvPtr(r.ConversationID),
				Continued:       cfg.Continued(),
				Model:           attemptModel,
				CWD:             effectiveWorkDir(cfg),
				Prompt:          cfg.Prompt,
				Artifacts:       []string{},
				Sandbox:         cfg.Sandbox,
				NetLog:          r.NetLog,
				ExitCode:        sigExit,
				ErrorClass:      "cancelled_by_signal",
			}
			index.Append(paths.IndexFile, rec)
		}

		if rc == config.ExitRateLimited && (r.Error == nil || r.Error.Reason != "cancelled_by_signal") {
			// モデル fallback と `--wait-quota` の分岐に入る前にも `signalFromContext(ctx)` を確認し、受信済みなら同じ結果を返す
			if sig := signalFromContext(ctx); sig != nil {
				sigNum := 0
				if sysSig, ok := sig.(syscall.Signal); ok {
					sigNum = int(sysSig)
				}
				sigExit := config.ExitForSignal(syscall.Signal(sigNum))
				r.Error = &emit.Error{
					Reason: "cancelled_by_signal",
					Detail: signalName(sig),
				}
				r.ExitCode = sigExit
				appendInterruptedIndex(sigExit)
				r.Warnings = prependWarning(r.Warnings, preflight)
				for _, w := range r.Warnings {
					e.Note("warning: " + w)
				}
				e.Emit(r)
				return sigExit
			}

			// Model fallback: try the next candidate from -m "A,B,...".
			if i < len(models)-1 {
				e.Note(fmt.Sprintf("rate_limited: %s が上限。次候補 %s で再試行します。",
					modelLabel(model), modelLabel(models[i+1])))
				i++
				continue
			}
			// Quota-reset wait (opt-in, once, bounded).
			if cfg.WaitQuota && !waited {
				if d, ok := ratelimit.ResetWait(logFile); ok {
					limit := time.Duration(cfg.MaxQuotaWait) * time.Second
					if d <= limit {
						e.Note(fmt.Sprintf("--wait-quota: リセットまで %s 待機して再試行します。", d))
						if err := waitQuotaSleep(ctx, d); err != nil {
							if sig := signalFromContext(ctx); sig != nil {
								sigNum := 0
								if sysSig, ok := sig.(syscall.Signal); ok {
									sigNum = int(sysSig)
								}
								sigExit := config.ExitForSignal(syscall.Signal(sigNum))
								r.Error = &emit.Error{
									Reason: "cancelled_by_signal",
									Detail: signalName(sig),
								}
								r.ExitCode = sigExit
								appendInterruptedIndex(sigExit)
								r.Warnings = prependWarning(r.Warnings, preflight)
								for _, w := range r.Warnings {
									e.Note("warning: " + w)
								}
								e.Emit(r)
								return sigExit
							}
							return rc
						}
						waited = true
						if r.Error != nil {
							r.Error.WaitedSec = int(d.Seconds())
						}
						continue
					}
					e.Note(fmt.Sprintf("--wait-quota: リセット待ち %s が上限 %s を超過。待機せず rate_limited を返します。", d, limit))
				}
			}
			// Exhausted: annotate what was tried so the caller can decide.
			if r.Error != nil {
				if tried := nonEmpty(models); len(tried) > 1 {
					r.Error.ModelsTried = tried
				}
			}
		}

		r.Warnings = prependWarning(r.Warnings, preflight)
		for _, w := range r.Warnings {
			e.Note("warning: " + w)
		}
		e.Emit(r)
		return rc
	}
}

// attempt runs agy once on the given model under the watchdog, records the
// invocation in index.jsonl, and returns the assembled result plus the resolved
// exit code and log path. It never emits the contract object — execute() owns
// the single stdout write so retries do not duplicate it. Stall/timeout log
// tails still go to stderr here, since they are per-physical-run diagnostics.
func attempt(ctx context.Context, cfg config.Config, model string, opt Options, e *emit.Emitter, paths Paths) (emit.Result, int, string) {
	if err := os.MkdirAll(paths.LogDir, 0o755); err != nil {
		return errResult("logdir_error", err), config.ExitUsage, ""
	}
	start := timeNow()
	logFile := filepath.Join(paths.LogDir,
		fmt.Sprintf("agy-run-%s-%d.log", start.Format("20060102_150405"), os.Getpid()))

	bodyCap, err := os.CreateTemp("", "agy-run-body-*")
	if err != nil {
		return errResult("bodycap_error", err), config.ExitUsage, logFile
	}
	bodyPath := bodyCap.Name()
	defer func() { _ = os.Remove(bodyPath) }()

	args := buildArgs(cfg, model, logFile)
	tsStart := start.Format(time.RFC3339)

	devnull, _ := os.Open(os.DevNull)
	if devnull != nil {
		defer func() { _ = devnull.Close() }()
	}

	// runCtx is per-attempt (cancelled between fallback retries) but inherits the
	// signal-aware parent ctx, so a received signal cancels the in-flight agy too.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	agyArgv := append(args, "-p", cfg.Prompt)
	binPath := paths.AgyBin
	spawnArgv := agyArgv

	// Sandbox mode (default ON; --no-sandbox / AGY_RUN_SANDBOX=0 to opt out):
	// run agy under bwrap and log every HTTP(S) connection it makes (never
	// blocking/filtering — see internal/sandbox and README). When bwrap is not
	// on PATH, degrade to a plain (unsandboxed) run rather than hard-failing —
	// this must never block a run, so the caller sees a warning instead of an
	// error (issue #15).
	var netLogPath string
	var sandboxWarn string
	var extraFiles []*os.File
	if cfg.Sandbox {
		if _, err := exec.LookPath("bwrap"); err != nil {
			sandboxWarn = "bwrap (bubblewrap) が未導入のため、このrunは非サンドボックスで実行しました（FS隔離・通信記録なし）。bubblewrap を入れると --sandbox の隔離＋記録が有効になります。"
			cfg.Sandbox = false
		}
	}
	if cfg.Sandbox {
		netLogPath = filepath.Join(paths.LogDir, "net-access.jsonl")
		logW, ferr := sandbox.OpenLogFile(netLogPath)
		if ferr != nil {
			_ = bodyCap.Close()
			return errResult("sandbox_netlog_open_failed", ferr), config.ExitUsage, logFile
		}
		defer func() { _ = logW.Close() }()

		runID := fmt.Sprintf("%s-%d", start.Format("20060102_150405"), os.Getpid())
		proxy := sandbox.NewProxy(runID, logW)
		addr, perr := proxy.Start()
		if perr != nil {
			_ = bodyCap.Close()
			return errResult("sandbox_proxy_start_failed", perr), config.ExitUsage, logFile
		}
		defer func() { _ = proxy.Stop() }()

		// The availability check above already confirmed bwrap exists; look it
		// up again here to get its resolved path (cheap, avoids threading it
		// through).
		bwrapBin, lerr := exec.LookPath("bwrap")
		if lerr != nil {
			_ = bodyCap.Close()
			return errResult("bwrap_not_found", lerr), config.ExitUsage, logFile
		}

		sbOpt, ghToken := sandboxOptions(opt, effectiveWorkDir(cfg), "http://"+addr)

		// GH_TOKEN is handed to bwrap through a pipe, never through argv/env
		// of bwrap itself (issue #25 — /proc/<pid>/cmdline is world-readable
		// to same-uid processes, and bwrap's --setenv used to leak the value
		// there). sandbox.Options only ever learns the fd *number*: the pipe
		// read end becomes ExtraFiles[0], which Go always places at fd 3 in
		// the child (fds 0-2 are stdin/stdout/stderr).
		if ghToken != "" {
			pr, pw, perr := os.Pipe()
			if perr != nil {
				_ = bodyCap.Close()
				return errResult("gh_token_pipe_failed", perr), config.ExitUsage, logFile
			}
			defer func() { _ = pr.Close() }()
			go func(w *os.File, token string) {
				_, _ = io.WriteString(w, token)
				_ = w.Close()
			}(pw, ghToken)
			extraFiles = append(extraFiles, pr)
			sbOpt.GHTokenFD = 3
		}

		bwrapArgs := sandbox.BuildArgs(sbOpt)

		execArgv := append([]string{paths.AgyBin}, agyArgv...)
		if sbOpt.GHTokenFD > 0 {
			execArgv = sandbox.WrapExecWithSecretEnv(sandbox.GHTokenSecretPath, "GH_TOKEN", execArgv)
		}

		binPath = bwrapBin
		spawnArgv = append(append([]string{}, bwrapArgs...), "--")
		spawnArgv = append(spawnArgv, execArgv...)
	}

	// agy's stdout is captured (not forwarded live): the raw body is folded into
	// the final JSON result, so nothing but that one object reaches our stdout.
	cmd := commandContext(runCtx, binPath, spawnArgv)
	cmd.Stdin = devnull
	cmd.Stdout = bodyCap
	cmd.Stderr = opt.Stderr
	cmd.ExtraFiles = extraFiles
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// On any runCtx cancellation (a received signal, or normal teardown) kill
	// agy's WHOLE process group, not just the direct child, so no descendant is
	// orphaned (issue #7). WaitDelay caps how long Wait() blocks afterward when a
	// grandchild keeps an inherited output pipe open — this only arises when
	// Stderr is not a raw *os.File (e.g. the test harness); it is harmless in the
	// production path where Stderr is os.Stderr.
	var cancelCalled atomic.Bool
	cmd.Cancel = func() error {
		cancelCalled.Store(true)
		return killGroup(cmd.Process)
	}
	cmd.WaitDelay = 2 * time.Second
	if cfg.Workspace != "" {
		cmd.Dir = cfg.Workspace // pin agy's cwd so RUN_COMMAND resolves correctly (issue #1)
	}

	if err := ctx.Err(); err != nil {
		if sig := signalFromContext(ctx); sig != nil {
			_ = bodyCap.Close()
			sigNum := 0
			if sysSig, ok := sig.(syscall.Signal); ok {
				sigNum = int(sysSig)
			}
			sigExit := config.ExitForSignal(syscall.Signal(sigNum))
			r := emit.Result{
				ExitCode: sigExit,
				Error: &emit.Error{
					Reason: "cancelled_by_signal",
					Detail: signalName(sig),
				},
			}
			rec := index.Record{
				TSStart:         tsStart,
				TSEnd:           timeNow().Format(time.RFC3339),
				DurationMS:      timeNow().Sub(start).Milliseconds(),
				ClaudeSessionID: opt.Getenv("CLAUDE_CODE_SESSION_ID"),
				ChildSessionID:  opt.Getenv("CLAUDE_CODE_CHILD_SESSION"),
				Continued:       cfg.Continued(),
				Model:           model,
				CWD:             effectiveWorkDir(cfg),
				Prompt:          cfg.Prompt,
				Artifacts:       []string{},
				Sandbox:         cfg.Sandbox,
				NetLog:          netLogPath,
				ExitCode:        sigExit,
				ErrorClass:      r.Error.Reason,
			}
			index.Append(paths.IndexFile, rec)
			return r, sigExit, logFile
		}
	}

	if serr := cmd.Start(); serr != nil {
		_ = bodyCap.Close()
		return errResult("spawn_failed", serr), 127, logFile
	}

	// Watchdog: idle + absolute limits. It kills the process group on a limit.
	w := watchdog.Watcher{
		Start: start,
		Idle:  time.Duration(cfg.IdleLimitSec) * time.Second,
		Abs:   time.Duration(cfg.AbsLimitSec) * time.Second,
		Poll:  time.Duration(cfg.PollSec) * time.Second,
		Paths: heartbeatPaths(logFile, bodyPath, paths.BrainDir, logFile),
		Probe: func() watchdog.Reason { return authProbe(logFile, bodyPath) },
	}
	reasonCh := make(chan watchdog.Reason, 1)
	go func() { reasonCh <- w.Run(runCtx, func() { _ = killGroup(cmd.Process) }) }()

	waitErr := cmd.Wait()
	if afterWait != nil {
		afterWait(ctx)
	}
	cancel()
	reason := <-reasonCh
	_ = bodyCap.Close()

	end := timeNow()
	rc := exitCode(cmd, waitErr, reason)

	convID := convlog.ResolveConvID(logFile)
	body := readBody(bodyPath)

	var effectiveModel string
	if model != "" {
		effectiveModel = model
	} else {
		tp := convlog.TranscriptPath(paths.BrainDir, convID)
		_, modelMap, _ := listModelMapFunc(paths.AgyBin)
		effectiveModel = convlog.ResolveEffectiveModel(model, tp, paths.Settings, modelMap)
	}

	rec := index.Record{
		TSStart:         tsStart,
		TSEnd:           end.Format(time.RFC3339),
		DurationMS:      end.Sub(start).Milliseconds(),
		ClaudeSessionID: opt.Getenv("CLAUDE_CODE_SESSION_ID"),
		ChildSessionID:  opt.Getenv("CLAUDE_CODE_CHILD_SESSION"),
		ConversationID:  index.ConvPtr(convID),
		Continued:       cfg.Continued(),
		Model:           effectiveModel,
		CWD:             effectiveWorkDir(cfg),
		Prompt:          cfg.Prompt,
		Artifacts:       []string{},
		Sandbox:         cfg.Sandbox,
		NetLog:          netLogPath,
	}

	// Assemble the single result object shared by every exit path.
	r := emit.Result{Body: body}
	if sandboxWarn != "" {
		r.Warnings = append(r.Warnings, sandboxWarn)
	}
	if model != "" {
		r.Model = model
	}
	if convID != "" {
		r.ConversationID = convID
	}
	if netLogPath != "" {
		r.NetLog = netLogPath
	}

	// artifacts: new images written under brain/<conv>/ during this run.
	arts := convlog.NewImageArtifacts(paths.BrainDir, convID, start)
	if len(arts) > 0 {
		rec.Artifacts = arts
		r.Artifacts = arts
	}

	// Stalled tool call detection (record warning, do not stop execution).
	if tp := convlog.TranscriptPath(paths.BrainDir, convID); tp != "" {
		if warn := convlog.DetectToolStall(tp, r.Artifacts, cfg.ToolRepeatLimit, start); warn != "" {
			r.Warnings = append(r.Warnings, warn)
		}
	}

	// Stall / absolute-timeout / auth-required branches: log tail to stderr, error in the result.
	if reason == watchdog.Stall || reason == watchdog.AbsTimeout || reason == watchdog.AuthRequired {
		touch(paths.KillStamp)
		printLogTail(e, logFile)
		detail := ""
		if reason == watchdog.AuthRequired {
			detail, _ = authreq.Detect(logFile)
		}
		r.Error = killError(reason, cfg, detail)
		r.ExitCode = rc
		rec.ExitCode = rc
		rec.ErrorClass = r.Error.Reason
		index.Append(paths.IndexFile, rec)
		return r, rc, logFile
	}

	// Cancelled by signal: agy-run received SIGINT/SIGTERM/SIGHUP and killed the agy process group.
	if sig := signalFromContext(ctx); sig != nil && cancelCalled.Load() {
		sigNum := 0
		if sysSig, ok := sig.(syscall.Signal); ok {
			sigNum = int(sysSig)
		}
		sigExit := config.ExitForSignal(syscall.Signal(sigNum))
		r.Error = &emit.Error{
			Reason: "cancelled_by_signal",
			Detail: signalName(sig),
		}
		r.ExitCode = sigExit
		rec.ExitCode = sigExit
		rec.ErrorClass = r.Error.Reason
		index.Append(paths.IndexFile, rec)
		return r, sigExit, logFile
	}

	// body_file: copy the captured body to a stable per-conversation path.
	if len(body) > 0 && convID != "" {
		if dst, ok := copyBody(paths.LastBodyDir, convID, bodyPath); ok {
			r.BodyFile = dst
		}
	}

	// RUN_COMMAND audit: surface agy's failed build/test steps so a caller does
	// not trust a self-reported "tests are green" (issue #4).
	if fails := runcmd.Scan(logFile); len(fails) > 0 {
		r.RunCommandFailures = len(fails)
		r.Warnings = append(r.Warnings, fmt.Sprintf(
			"agy の RUN_COMMAND(build/test) が %d 件失敗しています。agy の自己申告（テスト緑等）は要再検証。",
			len(fails)))
	}

	// Auth-required classification (post-exit): if agy exited on its own, but log has
	// the interactive OAuth transition line.
	// Precedence: after watchdog kill, before rate_limited.
	if authLine, ok := authreq.Detect(logFile); ok {
		if len(body) == 0 {
			r.Error = &emit.Error{
				Reason: "auth_required",
				Detail: authLine,
				Hint:   authRequiredHint,
			}
			rc = config.ExitAuthRequired
		} else {
			r.Warnings = append(r.Warnings, fmt.Sprintf(
				"本文は返っていますがログに interactive OAuth の兆候があります (%s)。exit_code は変更されていません。",
				authLine))
		}
	}

	// Rate-limit / artifact-missing / empty-output / generic-failure classification.
	if hits := ratelimit.CountHits(logFile); hits > 0 {
		if len(body) == 0 {
			if r.Error == nil {
				r.Error = &emit.Error{
					Reason: "rate_limited", LogRatelimitHits: hits,
					Hint: "agyの5時間枠/レート上限の可能性。-m にフォールバック候補を並べるか --wait-quota でリセット待機を検討。",
				}
				rc = config.ExitRateLimited // contract: 42 = empty body + rate-limit signature, whatever agy's own exit code was.
			}
		} else {
			r.Warnings = append(r.Warnings, fmt.Sprintf(
				"本文は返っていますがログにレート制限の兆候があります (レート制限シグネチャのヒット数: %d)。exit_code は変更されていません。",
				hits))
		}
	}

	// Artifact-missing classification (only if not rate_limited and not auth_required).
	if r.Error == nil {
		if tp := convlog.TranscriptPath(paths.BrainDir, convID); tp != "" {
			toolCalls := convlog.CountArtifactToolCalls(tp, start)
			if err := convlog.CheckArtifactMissing(toolCalls, r.Artifacts); err != nil {
				r.Error = err
				rc = config.ExitArtifactMissing
			}
		}
	}

	if len(body) == 0 && rc == config.ExitOK && r.Error == nil {
		r.Error = &emit.Error{
			Reason: "empty_output",
			Note:   "本文ゼロ。stderr のログ末尾を確認（rc は変更しない）。",
		}
	}

	if rc != config.ExitOK && r.Error == nil {
		errObj := &emit.Error{Reason: "agy_error"}
		if rc == 137 && waitErr != nil {
			errObj.Detail = waitErr.Error()
		}
		r.Error = errObj
	}

	r.ExitCode = rc
	rec.ExitCode = rc
	if r.Error != nil {
		rec.ErrorClass = r.Error.Reason
	} else if rec.ExitCode != 0 {
		// Defensive net: should be unreachable given the classification above
		// (any rc!=0 gets a Reason there), but keeps index.jsonl's error_class
		// from silently going blank on a failing run if a future code path
		// reaches here without setting r.Error (issue #22 follow-up).
		rec.ErrorClass = "agy_error"
	}
	index.Append(paths.IndexFile, rec)
	return r, rc, logFile
}

// errResult builds a terminal error Result; the caller supplies the exit code.
func errResult(reason string, err error) emit.Result {
	return emit.Result{Error: &emit.Error{Reason: reason, Detail: err.Error()}}
}

// effectiveWorkDir is the directory the run operates from: -W when set,
// otherwise the caller's cwd.
func effectiveWorkDir(cfg config.Config) string {
	if cfg.Workspace != "" {
		return cfg.Workspace
	}
	if d, err := os.Getwd(); err == nil {
		return d
	}
	return ""
}

// modelLabel renders a model name for human notes ("" -> agy's default).
func modelLabel(model string) string {
	if model == "" {
		return "(default)"
	}
	return `"` + model + `"`
}

// prependWarning puts a non-empty warning at the head of ws (invocation-level
// preflight warnings should read before per-run ones).
func prependWarning(ws []string, w string) []string {
	if w == "" {
		return ws
	}
	return append([]string{w}, ws...)
}

// nonEmpty drops "" entries (the no-model placeholder) from a model list.
func nonEmpty(models []string) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		if m != "" {
			out = append(out, m)
		}
	}
	return out
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

// authProbe checks whether agy triggered an interactive OAuth flow while
// running in non-interactive mode. To prevent TOCTOU races, it scans the log
// for the signature line FIRST, and only if a match is found does it check the
// body capture size — returning AuthRequired if the body is 0 bytes (or not yet
// created), and None otherwise.
func authProbe(logFile, bodyPath string) watchdog.Reason {
	if _, ok := authreq.Detect(logFile); !ok {
		return watchdog.None
	}
	fi, err := os.Stat(bodyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return watchdog.AuthRequired
		}
		return watchdog.None
	}
	if fi.Size() == 0 {
		return watchdog.AuthRequired
	}
	return watchdog.None
}

// killError builds the structured error for a stall (exit 44), absolute
// timeout (exit 124), or auth_required (exit 45), pointing the reader at the
// stderr log tail.
func killError(reason watchdog.Reason, cfg config.Config, detail string) *emit.Error {
	switch reason {
	case watchdog.Stall:
		return &emit.Error{
			Reason:       "stall",
			IdleLimitSec: cfg.IdleLimitSec,
			Note:         "heartbeat 途絶（agy 応答停止）。stderr にログ末尾を出力。",
		}
	case watchdog.AbsTimeout:
		return &emit.Error{
			Reason:       "timeout",
			AbsLimitSec:  cfg.AbsLimitSec,
			PrintTimeout: cfg.PrintTimeout,
			Note:         "絶対上限に到達。stderr にログ末尾を出力。",
		}
	case watchdog.AuthRequired:
		return &emit.Error{
			Reason: "auth_required",
			Detail: detail,
			Hint:   authRequiredHint,
		}
	}
	return &emit.Error{Reason: "unknown"}
}

// printLogTail writes the last few log lines (free text) to stderr for the
// stall/timeout diagnostics.
func printLogTail(e *emit.Emitter, logFile string) {
	e.Plain("\n")
	if tail := tailLines(logFile, 5); tail != "" {
		e.Plain(tail)
	}
}

var listModelMapFunc = listModelMap

func listModelMap(agyBin string) (models []string, modelMap map[string]string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := commandContext(ctx, agyBin, []string{"models"}).Output()
	if err != nil {
		return nil, nil, false
	}
	models, modelMap = parseModelsAndMap(string(out))
	return models, modelMap, len(models) > 0
}

func parseModelsAndMap(out string) ([]string, map[string]string) {
	var models []string
	modelMap := make(map[string]string)
	lines := strings.Split(out, "\n")
	hasTab := false
	for _, ln := range lines {
		if strings.Contains(ln, "\t") {
			hasTab = true
			break
		}
	}
	for _, ln := range lines {
		ln = strings.TrimRight(ln, "\r")
		if hasTab {
			if id, name, found := strings.Cut(ln, "\t"); found {
				id = strings.TrimSpace(id)
				name = strings.TrimSpace(name)
				if id != "" {
					models = append(models, id)
					if name != "" {
						modelMap[name] = id
					}
					modelMap[id] = id
				}
			}
		} else {
			if s := strings.TrimSpace(ln); s != "" {
				models = append(models, s)
				modelMap[s] = s
			}
		}
	}
	return models, modelMap
}

// listModels returns the model names from `agy models` (one per line). ok is
// false on any error so the caller can skip validation rather than block.
func listModels(agyBin string) (models []string, ok bool) {
	models, _, ok = listModelMapFunc(agyBin)
	return models, ok
}

// contains reports whether s is exactly one of items.
func contains(items []string, s string) bool {
	for _, it := range items {
		if it == s {
			return true
		}
	}
	return false
}

// readBody returns the captured agy body, or "" if it cannot be read.
func readBody(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// buildArgs assembles the agy argument list (excluding the trailing -p prompt)
// for the given model (the resolved fallback candidate, possibly "").
func buildArgs(cfg config.Config, model, logFile string) []string {
	args := []string{"--print-timeout", cfg.PrintTimeout, "--log-file", logFile}
	if model != "" {
		args = append(args, "--model", model)
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
	case watchdog.AuthRequired:
		return config.ExitAuthRequired
	}
	if cmd.ProcessState != nil {
		if code := cmd.ProcessState.ExitCode(); code >= 0 {
			return code
		}
	}
	if waitErr == nil {
		return 0
	}
	return 137 // agy-run はシグナルを受けておらず、agy だけが外から殺された
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

// killGroup sends SIGKILL to the whole process group (Setpgid gave the child its
// own group whose id equals its pid), ensuring agy and its children all die. It
// returns os.ErrProcessDone when the process/group is already gone, matching the
// exec.Cmd.Cancel convention; the watchdog callback ignores the return.
func killGroup(p *os.Process) error {
	if p == nil {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}
