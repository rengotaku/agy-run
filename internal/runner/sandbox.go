package runner

import (
	"os"
	"strconv"

	"github.com/rengotaku/agy-run/internal/sandbox"
)

// sandboxOptions resolves the bwrap Options for one attempt from already
// known runtime values (opt.Home, opt.Getenv) plus the cwd this attempt runs
// in and the local net-logger address it must forward to. XDG_RUNTIME_DIR
// falls back to /run/user/<uid> when unset, matching the PoC-verified
// default (issue #11). GH_TOKEN falls back to GITHUB_TOKEN when unset, so
// gh/git GitHub auth works inside the sandbox (issue #17); it stays "" (not
// forwarded) when neither is set.
func sandboxOptions(opt Options, cwd, proxyAddr string) sandbox.Options {
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
	return sandbox.Options{
		Home:          opt.Home,
		Cwd:           cwd,
		XDGRuntimeDir: xdgRuntime,
		DBusAddress:   getenv("DBUS_SESSION_BUS_ADDRESS"),
		PathEnv:       getenv("PATH"),
		ProxyAddr:     proxyAddr,
		GHToken:       ghToken,
		ExtraROBinds:  sandbox.ParseBindList(getenv("AGY_RUN_SANDBOX_RO_BINDS")),
		ExtraRWBinds:  sandbox.ParseBindList(getenv("AGY_RUN_SANDBOX_RW_BINDS")),
	}
}
