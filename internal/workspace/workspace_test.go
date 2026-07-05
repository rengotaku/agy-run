package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setup builds a container with main/ and feat/ worktrees plus a settings.json
// trusting only main/, and returns their paths.
func setup(t *testing.T) (container, mainWT, featWT, settings string) {
	t.Helper()
	container = t.TempDir()
	mainWT = filepath.Join(container, "main")
	featWT = filepath.Join(container, "feat")
	for _, d := range []string{mainWT, featWT} {
		if err := os.MkdirAll(filepath.Join(d, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	settings = filepath.Join(t.TempDir(), "settings.json")
	body := `{"trustedWorkspaces":["` + mainWT + `"]}`
	if err := os.WriteFile(settings, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return container, mainWT, featWT, settings
}

func TestPreflightWarnsOnSiblingWorktree(t *testing.T) {
	_, mainWT, featWT, settings := setup(t)
	w := Preflight(featWT, settings)
	if w == "" || !strings.Contains(w, mainWT) {
		t.Errorf("expected warning naming main worktree, got %q", w)
	}
}

func TestPreflightSilentWhenInTrusted(t *testing.T) {
	_, mainWT, _, settings := setup(t)
	if w := Preflight(mainWT, settings); w != "" {
		t.Errorf("no warning expected when working in the trusted tree, got %q", w)
	}
}

func TestPreflightSilentWhenNestedUnderTrusted(t *testing.T) {
	_, mainWT, _, settings := setup(t)
	sub := filepath.Join(mainWT, "cmd", "app")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if w := Preflight(sub, settings); w != "" {
		t.Errorf("nested dir of trusted tree should not warn, got %q", w)
	}
}

func TestPreflightSilentForUnrelatedRepo(t *testing.T) {
	_, _, _, settings := setup(t)
	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(other, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if w := Preflight(other, settings); w != "" {
		t.Errorf("unrelated repo (no sibling trust) should not warn, got %q", w)
	}
}

func TestPreflightSilentWithoutGitRoot(t *testing.T) {
	_, _, _, settings := setup(t)
	if w := Preflight(t.TempDir(), settings); w != "" {
		t.Errorf("non-git dir should not warn, got %q", w)
	}
}

func TestPreflightSilentWithoutSettings(t *testing.T) {
	_, _, featWT, _ := setup(t)
	if w := Preflight(featWT, filepath.Join(t.TempDir(), "missing.json")); w != "" {
		t.Errorf("missing settings should not warn, got %q", w)
	}
}
