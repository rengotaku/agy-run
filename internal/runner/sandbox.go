package runner

import (
	"os"
	"strconv"
	"strings"

	"github.com/rengotaku/agy-run/internal/sandbox"
)

// sandboxOptions resolves the bwrap Options for one attempt from already
// known runtime values (opt.Home, opt.Getenv) plus the cwd this attempt runs
// in and the local net-logger address it must forward to. XDG_RUNTIME_DIR
// falls back to /run/user/<uid> when unset, matching the PoC-verified
// default (issue #11). GH_TOKEN falls back to GITHUB_TOKEN when unset, so
// gh/git GitHub auth works inside the sandbox (issue #17); it stays "" (not
// forwarded) when neither is set.
//
// The resolved token value is returned separately from sandbox.Options (never
// as a field on it — issue #25): sandbox.Options only ever carries a
// GHTokenFD (an fd number, set by the caller once it has piped the value in),
// so the sandbox package itself is structurally unable to place the token's
// value in bwrap's argv. The caller (attempt(), in runner.go) is responsible
// for turning the returned string into a pipe and wiring GHTokenFD.
//
// AGY_RUN_SANDBOX_NO_DBUS=1 opts out of forwarding the D-Bus session socket
// into the sandbox (issue #19). The default forwards it because agy's OAuth
// token lives in Secret Service (gnome-keyring) and unauthenticated runs die
// with "Authentication required" (bisected in issue #11). Setting the opt-in
// closes the systemd-run --user escape path that the D-Bus bind otherwise
// exposes when -d (--dangerously-skip-permissions) is active — at the cost
// of breaking Secret Service-backed auth. Users whose agy credentials are in
// ~/.gemini directly (or who re-auth per session) can safely enable it.
func sandboxOptions(opt Options, cwd, proxyAddr string) (sandbox.Options, string) {
	getenv := opt.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	xdgRuntime := getenv("XDG_RUNTIME_DIR")
	if xdgRuntime == "" {
		xdgRuntime = "/run/user/" + strconv.Itoa(os.Getuid())
	}
	ghToken := getenv("GH_TOKEN")
	if ghToken == "" {
		ghToken = getenv("GITHUB_TOKEN")
	}
	dbusAddr := getenv("DBUS_SESSION_BUS_ADDRESS")
	if envBool(getenv, "AGY_RUN_SANDBOX_NO_DBUS") {
		dbusAddr = ""
	}
	return sandbox.Options{
		Home:          opt.Home,
		Cwd:           cwd,
		XDGRuntimeDir: xdgRuntime,
		DBusAddress:   dbusAddr,
		PathEnv:       getenv("PATH"),
		ProxyAddr:     proxyAddr,
		ExtraROBinds:  sandbox.ParseBindList(getenv("AGY_RUN_SANDBOX_RO_BINDS")),
		ExtraRWBinds:  sandbox.ParseBindList(getenv("AGY_RUN_SANDBOX_RW_BINDS")),
	}, ghToken
}

// envBool matches the semantics of config.envBool for a single boolean flag:
// a value of "1", "true", "yes", or "on" (case-insensitive) is truthy; empty
// or anything else is false. Kept local to avoid a runner→config dependency
// just for one string check.
func envBool(getenv func(string) string, key string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
