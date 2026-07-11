package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// containsSeq reports whether needle appears in a, in order, as a contiguous
// slice (like strings.Contains but for []string args).
func containsSeq(a, needle []string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(a); i++ {
		match := true
		for j := range needle {
			if a[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestBuildArgsStaticSystemFlags(t *testing.T) {
	home := t.TempDir()
	args := BuildArgs(Options{Home: home})

	for _, want := range [][]string{
		{"--unshare-all", "--share-net"},
		{"--ro-bind", "/usr", "/usr"},
		{"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp"},
		{"--ro-bind-try", "/etc/resolv.conf", "/etc/resolv.conf"},
		{"--ro-bind-try", "/etc/hosts", "/etc/hosts"},
		{"--ro-bind-try", "/etc/passwd", "/etc/passwd"},
		{"--ro-bind-try", "/etc/group", "/etc/group"},
		{"--ro-bind-try", "/etc/machine-id", "/etc/machine-id"},
		{"--clearenv"},
	} {
		if !containsSeq(args, want) {
			t.Errorf("args missing %v\nfull args: %v", want, args)
		}
	}
}

func TestBuildArgsHomeCwdEnvTail(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()

	got := BuildArgs(Options{
		Home:          home,
		Cwd:           cwd,
		XDGRuntimeDir: "/run/user/1000",
		DBusAddress:   "unix:path=/run/user/1000/bus,guid=abc",
		PathEnv:       "/usr/bin:/bin",
		ProxyAddr:     "http://127.0.0.1:12345",
	})

	// D-Bus socket bind (extracted from DBUS_SESSION_BUS_ADDRESS).
	if !containsSeq(got, []string{"--bind", "/run/user/1000/bus", "/run/user/1000/bus"}) {
		t.Errorf("missing D-Bus socket bind\nargs: %v", got)
	}
	// cwd bind + chdir.
	if !containsSeq(got, []string{"--bind", cwd, cwd}) {
		t.Errorf("missing cwd bind\nargs: %v", got)
	}
	if !containsSeq(got, []string{"--chdir", cwd}) {
		t.Errorf("missing --chdir\nargs: %v", got)
	}

	// The clearenv+setenv tail is fully deterministic from Options — assert it
	// exactly (golden), since none of it depends on the host's own filesystem.
	want := []string{
		"--clearenv",
		"--setenv", "HOME", home,
		"--setenv", "PATH", "/usr/bin:/bin",
		"--setenv", "XDG_RUNTIME_DIR", "/run/user/1000",
		"--setenv", "DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus,guid=abc",
		"--setenv", "HTTPS_PROXY", "http://127.0.0.1:12345",
		"--setenv", "HTTP_PROXY", "http://127.0.0.1:12345",
		"--setenv", "https_proxy", "http://127.0.0.1:12345",
		"--setenv", "http_proxy", "http://127.0.0.1:12345",
		"--setenv", "TERM", "dumb",
	}
	tail := got[len(got)-len(want):]
	if len(tail) != len(want) {
		t.Fatalf("tail length = %d, want %d\ntail: %v", len(tail), len(want), tail)
	}
	for i := range want {
		if tail[i] != want[i] {
			t.Errorf("tail[%d] = %q, want %q\nfull tail: %v", i, tail[i], want[i], tail)
		}
	}
}

func TestBuildArgsNoDBusAddressOmitsSocketBindAndEnv(t *testing.T) {
	got := BuildArgs(Options{Home: t.TempDir(), ProxyAddr: "http://127.0.0.1:1"})
	for _, s := range got {
		if s == "DBUS_SESSION_BUS_ADDRESS" {
			t.Fatalf("DBUS_SESSION_BUS_ADDRESS should not appear when DBusAddress is empty: %v", got)
		}
	}
}

func TestBuildArgsExtraBinds(t *testing.T) {
	home := t.TempDir()
	got := BuildArgs(Options{
		Home:         home,
		ExtraROBinds: []string{"/a/ro"},
		ExtraRWBinds: []string{"/b/rw"},
	})
	if !containsSeq(got, []string{"--ro-bind", "/a/ro", "/a/ro"}) {
		t.Errorf("missing extra ro bind\nargs: %v", got)
	}
	if !containsSeq(got, []string{"--bind", "/b/rw", "/b/rw"}) {
		t.Errorf("missing extra rw bind\nargs: %v", got)
	}
}

func TestBuildArgsGeminiConfigSymlinksResolveToRealPaths(t *testing.T) {
	home := t.TempDir()
	realTarget := t.TempDir() // simulates a dotfiles checkout living elsewhere
	skillsReal := filepath.Join(realTarget, "skills.json")
	if err := os.WriteFile(skillsReal, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgDir := filepath.Join(home, ".gemini", "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(skillsReal, filepath.Join(cfgDir, "skills.json")); err != nil {
		t.Fatal(err)
	}

	got := BuildArgs(Options{Home: home})
	if !containsSeq(got, []string{"--ro-bind", skillsReal, skillsReal}) {
		t.Errorf("expected ro-bind of resolved symlink target %s\nargs: %v", skillsReal, got)
	}
	// The dangling symlink path itself must not be bound (only the resolved target).
	danglingArg := filepath.Join(cfgDir, "skills.json")
	if containsSeq(got, []string{"--ro-bind", danglingArg, danglingArg}) {
		t.Errorf("did not expect a bind of the symlink path itself\nargs: %v", got)
	}
}

func TestBuildArgsGitWorktreeBindsGitdirAndCommondir(t *testing.T) {
	container := t.TempDir()
	mainRepo := filepath.Join(container, "main")
	worktree := filepath.Join(container, "feat")
	gitdir := filepath.Join(mainRepo, ".git", "worktrees", "feat")

	for _, d := range []string{mainRepo, worktree, gitdir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commonDir := filepath.Join(mainRepo, ".git")
	if err := os.WriteFile(filepath.Join(gitdir, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := BuildArgs(Options{Home: t.TempDir(), Cwd: worktree})

	if !containsSeq(got, []string{"--bind", worktree, worktree}) {
		t.Errorf("missing cwd bind\nargs: %v", got)
	}
	if !containsSeq(got, []string{"--bind", gitdir, gitdir}) {
		t.Errorf("missing gitdir bind\nargs: %v", got)
	}
	if !containsSeq(got, []string{"--bind", commonDir, commonDir}) {
		t.Errorf("missing commondir bind\nargs: %v", got)
	}
}

func TestBuildArgsGitWorktreeOutsideContainerNotBound(t *testing.T) {
	root := t.TempDir()
	container := filepath.Join(root, "container")
	worktree := filepath.Join(container, "feat")
	// gitdir resolves under a sibling of container (not under it) — must not
	// be trusted/bound (only gitdir/commondir under cwd's parent are).
	elsewhereRoot := filepath.Join(root, "elsewhere-root")
	gitdir := filepath.Join(elsewhereRoot, "some-gitdir")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(gitdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := BuildArgs(Options{Home: t.TempDir(), Cwd: worktree})
	if containsSeq(got, []string{"--bind", gitdir, gitdir}) {
		t.Errorf("did not expect gitdir outside container to be bound\nargs: %v", got)
	}
}

func TestParseBindListFiltersMissingPaths(t *testing.T) {
	existing := t.TempDir()
	if got := ParseBindList(""); got != nil {
		t.Errorf("empty input = %v, want nil", got)
	}
	got := ParseBindList(existing + ":/definitely/does/not/exist/xyz")
	if len(got) != 1 || got[0] != existing {
		t.Errorf("ParseBindList = %v, want [%s]", got, existing)
	}
}

func TestDbusSocketPath(t *testing.T) {
	cases := map[string]string{
		"unix:path=/run/user/1000/bus,guid=abc": "/run/user/1000/bus",
		"unix:path=/run/user/1000/bus":          "/run/user/1000/bus",
		"":                                      "",
		"tcp:host=127.0.0.1,port=1234":          "",
		"unix:abstract=/foo,guid=abc":           "",
	}
	for in, want := range cases {
		if got := dbusSocketPath(in); got != want {
			t.Errorf("dbusSocketPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWithinDir(t *testing.T) {
	if !withinDir("/a/b", "/a/b") {
		t.Error("base itself should be within")
	}
	if !withinDir("/a/b", "/a/b/c") {
		t.Error("child should be within")
	}
	if withinDir("/a/b", "/a/bc") {
		t.Error("sibling with shared prefix should not be within")
	}
	if withinDir("/a/b", "/c/d") {
		t.Error("unrelated path should not be within")
	}
}

func TestBuildArgsBinLibSymlinkOrRoBind(t *testing.T) {
	// /bin, /lib, /lib64, /sbin vary by distro (usrmerge symlink vs real dir);
	// whichever this host has, BuildArgs must faithfully mirror it: a
	// --symlink for a symlink, a --ro-bind for a real directory, nothing for a
	// path that does not exist on this host at all.
	got := BuildArgs(Options{Home: t.TempDir()})
	for _, ln := range []string{"/bin", "/lib", "/lib64", "/sbin"} {
		info, err := os.Lstat(ln)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			real, everr := filepath.EvalSymlinks(ln)
			if everr != nil {
				continue
			}
			if !containsSeq(got, []string{"--symlink", real, ln}) {
				t.Errorf("expected --symlink %s %s\nargs: %v", real, ln, got)
			}
		} else if info.IsDir() {
			if !containsSeq(got, []string{"--ro-bind", ln, ln}) {
				t.Errorf("expected --ro-bind %s %s\nargs: %v", ln, ln, got)
			}
		}
	}
}

func TestBuildArgsEmptyOptionsProducesNoHomeOrCwdBinds(t *testing.T) {
	got := BuildArgs(Options{})
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "--chdir") {
		t.Errorf("no cwd given, --chdir should be absent: %v", got)
	}
}
