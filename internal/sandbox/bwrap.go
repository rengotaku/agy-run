// Package sandbox implements bwrap (bubblewrap) filesystem isolation for a
// sandboxed agy run, plus a logging-only forward proxy (netlog.go) that
// records every HTTP(S) connection made through it without ever blocking or
// filtering (see the accompanying "sandbox" section in README.md and issue
// #11 for the PoC this codifies).
package sandbox

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Options are the already-resolved runtime values BuildArgs needs. Callers
// (internal/runner) gather these from os.UserHomeDir / env / the run's cwd;
// BuildArgs itself stays a pure function of Options plus whatever it reads
// from Home/Cwd to resolve the PoC-verified bind list (git-worktree
// gitdir/commondir, ~/.gemini/config symlink targets).
type Options struct {
	Home          string // os.UserHomeDir()
	Cwd           string // working directory to sandbox into (rw bind + --chdir)
	XDGRuntimeDir string // XDG_RUNTIME_DIR to export inside the sandbox
	DBusAddress   string // DBUS_SESSION_BUS_ADDRESS ("" = not forwarded/bound)
	PathEnv       string // PATH to forward (--setenv PATH)
	ProxyAddr     string // "http://127.0.0.1:<port>" — set on all 4 proxy env vars
	GHToken       string // GH_TOKEN to re-export inside the sandbox for gh/git GitHub auth ("" = not forwarded)
	ExtraROBinds  []string
	ExtraRWBinds  []string
}

// BuildArgs assembles the bwrap argv (everything before "-- <agy argv>") from
// the PoC-verified minimal layout (issue #11 "動作した bwrap 構成（最小）"):
//
//	--unshare-all --share-net
//	--ro-bind /usr /usr (+ /bin /lib /lib64 /sbin as --symlink or --ro-bind)
//	--proc /proc --dev /dev --tmpfs /tmp
//	--ro-bind /etc/ssl, --ro-bind-try resolv.conf/hosts/passwd/group/machine-id
//	--ro-bind ~/.local ~/.local            (agy / agy-run binaries)
//	--bind ~/.gemini ~/.gemini              (auth / conversations / brain / slots, rw required)
//	--ro-bind <resolved symlink targets under ~/.gemini/config>
//	--bind <D-Bus session socket>            (Secret Service auth, rw required)
//	--bind <cwd> <cwd> [+ git-worktree gitdir/commondir] --chdir <cwd>
//	--clearenv --setenv HOME/PATH/XDG_RUNTIME_DIR/DBUS_SESSION_BUS_ADDRESS
//	--setenv HTTPS_PROXY/HTTP_PROXY/https_proxy/http_proxy <ProxyAddr>
//	--setenv TERM dumb
//	--setenv GH_TOKEN <GHToken>  (gh/git GitHub 認証用。空なら渡さない)
func BuildArgs(opt Options) []string {
	var a []string
	add := func(s ...string) { a = append(a, s...) }

	add("--unshare-all", "--share-net")

	add("--ro-bind", "/usr", "/usr")
	for _, ln := range []string{"/bin", "/lib", "/lib64", "/sbin"} {
		info, err := os.Lstat(ln)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if real, everr := filepath.EvalSymlinks(ln); everr == nil {
				add("--symlink", real, ln)
				continue
			}
		}
		if info.IsDir() {
			add("--ro-bind", ln, ln)
		}
	}

	add("--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp")

	if info, err := os.Stat("/etc/ssl"); err == nil && info.IsDir() {
		add("--ro-bind", "/etc/ssl", "/etc/ssl")
	}
	for _, f := range []string{"/etc/resolv.conf", "/etc/hosts", "/etc/passwd", "/etc/group", "/etc/machine-id"} {
		add("--ro-bind-try", f, f)
	}

	if opt.Home != "" {
		localDir := filepath.Join(opt.Home, ".local")
		if info, err := os.Stat(localDir); err == nil && info.IsDir() {
			add("--ro-bind", localDir, localDir)
		}

		geminiDir := filepath.Join(opt.Home, ".gemini")
		if info, err := os.Stat(geminiDir); err == nil && info.IsDir() {
			add("--bind", geminiDir, geminiDir)
		}

		for _, real := range geminiConfigROBinds(opt.Home) {
			add("--ro-bind", real, real)
		}
	}

	if sock := dbusSocketPath(opt.DBusAddress); sock != "" {
		add("--bind", sock, sock)
	}

	if opt.Cwd != "" {
		add("--bind", opt.Cwd, opt.Cwd)
		for _, b := range resolveGitWorktreeBinds(opt.Cwd) {
			add("--bind", b, b)
		}
	}

	for _, p := range opt.ExtraROBinds {
		add("--ro-bind", p, p)
	}
	for _, p := range opt.ExtraRWBinds {
		add("--bind", p, p)
	}

	if opt.Cwd != "" {
		add("--chdir", opt.Cwd)
	}

	add("--clearenv")
	setenv := func(k, v string) {
		if v != "" {
			add("--setenv", k, v)
		}
	}
	setenv("HOME", opt.Home)
	setenv("PATH", opt.PathEnv)
	setenv("XDG_RUNTIME_DIR", opt.XDGRuntimeDir)
	setenv("DBUS_SESSION_BUS_ADDRESS", opt.DBusAddress)
	setenv("HTTPS_PROXY", opt.ProxyAddr)
	setenv("HTTP_PROXY", opt.ProxyAddr)
	setenv("https_proxy", opt.ProxyAddr)
	setenv("http_proxy", opt.ProxyAddr)
	setenv("TERM", "dumb")
	setenv("GH_TOKEN", opt.GHToken)

	return a
}

// ParseBindList splits a colon-separated AGY_RUN_SANDBOX_RO_BINDS /
// AGY_RUN_SANDBOX_RW_BINDS value, silently dropping entries that do not exist
// on disk — a stale or typo'd extra-bind path must not abort the whole
// sandboxed run.
func ParseBindList(v string) []string {
	if v == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ":") {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		out = append(out, p)
	}
	return out
}

// geminiConfigROBinds resolves the real (symlink-target) path of every entry
// directly under <home>/.gemini/config — agy-run's dotfiles-managed files
// such as skills.json and rules/ are symlinks into a dotfiles checkout that
// can live anywhere, so the sandbox must ro-bind the resolved target rather
// than the dangling symlink itself.
func geminiConfigROBinds(home string) []string {
	dir := filepath.Join(home, ".gemini", "config")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var binds []string
	for _, ent := range entries {
		real, everr := filepath.EvalSymlinks(filepath.Join(dir, ent.Name()))
		if everr != nil {
			continue
		}
		binds = append(binds, real)
	}
	sort.Strings(binds)
	return binds
}

// resolveGitWorktreeBinds returns the extra rw binds a git worktree cwd needs
// beyond the cwd bind itself: the worktree's gitdir (under
// <main-repo>/.git/worktrees/<name>) and the shared commondir it points back
// to. Both live outside cwd, so git cannot operate without them being bound
// too. Mirroring symphony's sandbox approach, a resolved path is only bound
// when it falls under cwd's parent directory (the worktree container) — a
// gitdir/commondir resolving somewhere unexpected (e.g. outside the
// container) is treated as "not a worktree we recognize" and skipped rather
// than blindly bound.
func resolveGitWorktreeBinds(cwd string) []string {
	gitPath := filepath.Join(cwd, ".git")
	info, err := os.Lstat(gitPath)
	if err != nil || info.IsDir() {
		return nil // ordinary repo (.git is a dir), or no .git at all
	}
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return nil
	}
	const prefix = "gitdir: "
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, prefix) {
		return nil
	}
	gitdir := strings.TrimPrefix(line, prefix)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(cwd, gitdir)
	}
	gitdir = filepath.Clean(gitdir)

	container := filepath.Dir(cwd)
	var binds []string
	if withinDir(container, gitdir) {
		binds = append(binds, gitdir)
	}

	if cd, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
		common := strings.TrimSpace(string(cd))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitdir, common)
		}
		common = filepath.Clean(common)
		if withinDir(container, common) {
			binds = append(binds, common)
		}
	}
	return binds
}

// withinDir reports whether target is base itself or lies under it.
func withinDir(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// dbusSocketPath extracts the unix socket path from a
// DBUS_SESSION_BUS_ADDRESS value such as
// "unix:path=/run/user/1000/bus,guid=xxxx" (addresses may be ";"-separated;
// the first "unix:path=" entry wins). Returns "" when addr carries no usable
// unix socket path (empty, abstract socket, tcp: transport, etc.) — those
// cannot be bind-mounted and are left for --setenv to forward as-is.
func dbusSocketPath(addr string) string {
	for _, part := range strings.Split(addr, ";") {
		if !strings.HasPrefix(part, "unix:") {
			continue
		}
		for _, kv := range strings.Split(strings.TrimPrefix(part, "unix:"), ",") {
			if p, ok := strings.CutPrefix(kv, "path="); ok && p != "" {
				return p
			}
		}
	}
	return ""
}
