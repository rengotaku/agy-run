package runner

import "testing"

func TestSandboxOptionsXDGRuntimeDirFallback(t *testing.T) {
	env := map[string]string{"PATH": "/usr/bin"}
	getenv := func(k string) string { return env[k] }

	opt := sandboxOptions(Options{Home: "/home/x", Getenv: getenv}, "/work", "http://127.0.0.1:1")
	if opt.XDGRuntimeDir == "" || opt.XDGRuntimeDir == "/run/user/" {
		t.Errorf("XDGRuntimeDir fallback not set: %q", opt.XDGRuntimeDir)
	}
	if opt.Home != "/home/x" || opt.Cwd != "/work" || opt.ProxyAddr != "http://127.0.0.1:1" {
		t.Errorf("opt = %+v", opt)
	}
	if opt.PathEnv != "/usr/bin" {
		t.Errorf("PathEnv = %q, want /usr/bin", opt.PathEnv)
	}
}

func TestSandboxOptionsXDGRuntimeDirFromEnv(t *testing.T) {
	env := map[string]string{"XDG_RUNTIME_DIR": "/run/user/42", "DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/42/bus"}
	getenv := func(k string) string { return env[k] }

	opt := sandboxOptions(Options{Home: "/home/x", Getenv: getenv}, "/work", "http://127.0.0.1:1")
	if opt.XDGRuntimeDir != "/run/user/42" {
		t.Errorf("XDGRuntimeDir = %q, want /run/user/42", opt.XDGRuntimeDir)
	}
	if opt.DBusAddress != "unix:path=/run/user/42/bus" {
		t.Errorf("DBusAddress = %q", opt.DBusAddress)
	}
}

func TestSandboxOptionsGHTokenForwarded(t *testing.T) {
	env := map[string]string{"GH_TOKEN": "ghp_TESTDUMMY"}
	getenv := func(k string) string { return env[k] }

	opt := sandboxOptions(Options{Home: "/home/x", Getenv: getenv}, "/work", "http://127.0.0.1:1")
	if opt.GHToken != "ghp_TESTDUMMY" {
		t.Errorf("GHToken = %q, want ghp_TESTDUMMY", opt.GHToken)
	}
}

func TestSandboxOptionsGHTokenFallsBackToGithubToken(t *testing.T) {
	env := map[string]string{"GITHUB_TOKEN": "ghs_TESTDUMMY"}
	getenv := func(k string) string { return env[k] }

	opt := sandboxOptions(Options{Home: "/home/x", Getenv: getenv}, "/work", "http://127.0.0.1:1")
	if opt.GHToken != "ghs_TESTDUMMY" {
		t.Errorf("GHToken = %q, want ghs_TESTDUMMY (fallback from GITHUB_TOKEN)", opt.GHToken)
	}
}

func TestSandboxOptionsGHTokenEmptyWhenBothUnset(t *testing.T) {
	getenv := func(k string) string { return "" }

	opt := sandboxOptions(Options{Home: "/home/x", Getenv: getenv}, "/work", "http://127.0.0.1:1")
	if opt.GHToken != "" {
		t.Errorf("GHToken = %q, want empty when GH_TOKEN/GITHUB_TOKEN both unset", opt.GHToken)
	}
}
