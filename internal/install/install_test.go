package install

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	embedded "github.com/ableinc/coding-agent-loop"
)

// The embed directive is the whole point of this package — if it silently
// picked up an empty file the binary would install a no-op unit.
func TestServiceTemplateEmbedded(t *testing.T) {
	if len(serviceTemplate) == 0 {
		t.Fatal("embedded service template is empty")
	}
	got := string(serviceTemplate)
	for _, want := range []string{
		"[Unit]", "[Service]", "[Install]",
		"ExecStart=/opt/coding-agent-loop/bin/coding-agent-loop",
		"User={{USER}}", "Group={{GROUP}}", "{{HOME}}/.agent-loop",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("embedded template missing %q", want)
		}
	}
}

func TestRenderSubstitutesPlaceholders(t *testing.T) {
	unit := string(render(target{user: "alice", group: "alice", home: "/home/alice"}))
	if strings.Contains(unit, "{{") {
		t.Fatalf("render left a placeholder unsubstituted:\n%s", unit)
	}
	if !strings.Contains(unit, "User=alice") || !strings.Contains(unit, "Group=alice") {
		t.Errorf("expected User=alice / Group=alice, got:\n%s", unit)
	}
	if !strings.Contains(unit, "ReadWritePaths=/opt/coding-agent-loop /home/alice/.agent-loop /home/alice/.claude") {
		t.Errorf("expected the home-scoped ReadWritePaths entry, got:\n%s", unit)
	}
}

func TestResolveTargetUsesSudoUser(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("cannot look up the current user in this environment")
	}
	t.Setenv("SUDO_USER", me.Username)

	tg, err := resolveTarget()
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if tg.dedicated {
		t.Fatal("a real SUDO_USER should never resolve to the dedicated fallback account")
	}
	if tg.user != me.Username || tg.home != me.HomeDir {
		t.Errorf("resolveTarget = %+v, want user=%s home=%s", tg, me.Username, me.HomeDir)
	}
	wantUID, _ := strconv.Atoi(me.Uid)
	if tg.uid != wantUID {
		t.Errorf("uid = %d, want %d", tg.uid, wantUID)
	}
}

func TestResolveTargetFallsBackWithoutSudoUser(t *testing.T) {
	t.Setenv("SUDO_USER", "")

	tg, err := resolveTarget()
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if !tg.dedicated || tg.user != dedicatedUser {
		t.Errorf("resolveTarget without SUDO_USER = %+v, want the dedicated fallback account", tg)
	}
}

func TestResolveTargetIgnoresSudoUserRoot(t *testing.T) {
	// Logging in directly as root (no sudo) sets no SUDO_USER; but if it is
	// somehow "root", that is not a real operator account to run as either.
	t.Setenv("SUDO_USER", "root")

	tg, err := resolveTarget()
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if !tg.dedicated {
		t.Error("SUDO_USER=root should not be treated as a real operator account")
	}
}

func TestPreviewUnitProducesNoPlaceholders(t *testing.T) {
	unit, err := PreviewUnit()
	if err != nil {
		t.Fatalf("PreviewUnit: %v", err)
	}
	if strings.Contains(string(unit), "{{") {
		t.Errorf("PreviewUnit left a placeholder unsubstituted:\n%s", unit)
	}
}

func TestRunRequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; the non-root guard can't be exercised")
	}
	if err := Run(Options{}); err == nil {
		t.Fatal("Run should refuse to proceed without root")
	}
}

// The systemd unit's ExecStart always passes an explicit --config pointing at
// dest, and an explicit path is never allowed to fall back at runtime — so
// dest must exist after install even when the operator supplied nothing.
func TestInstallConfigFallsBackToEmbeddedWhenNoneSupplied(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "config.json")
	tg := target{uid: os.Getuid(), gid: os.Getgid()}

	if err := installConfig("", dest, tg, func(string, ...any) {}); err != nil {
		t.Fatalf("installConfig: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(embedded.Config) {
		t.Fatal("with no configPath, dest should be written with the embedded config")
	}
}

func TestInstallConfigCopiesSuppliedFile(t *testing.T) {
	src := filepath.Join(t.TempDir(), "mine.json")
	body := `{"github":{"owners":["mine"]}}`
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "config.json")
	tg := target{uid: os.Getuid(), gid: os.Getgid()}

	if err := installConfig(src, dest, tg, func(string, ...any) {}); err != nil {
		t.Fatalf("installConfig: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("dest = %q, want the supplied file's content %q", got, body)
	}
}

func TestInstallConfigLeavesExistingFileUntouched(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "config.json")
	existing := `{"github":{"owners":["already-here"]}}`
	if err := os.WriteFile(dest, []byte(existing), 0o640); err != nil {
		t.Fatal(err)
	}
	tg := target{uid: os.Getuid(), gid: os.Getgid()}

	if err := installConfig("", dest, tg, func(string, ...any) {}); err != nil {
		t.Fatalf("installConfig: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != existing {
		t.Fatal("a re-run of --install must never clobber an existing config")
	}
}

func TestUninstallRequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; the non-root guard can't be exercised")
	}
	if err := Uninstall(UninstallOptions{}); err == nil {
		t.Fatal("Uninstall should refuse to proceed without root")
	}
}

func TestExpandHomeResolvesAgainstGivenHome(t *testing.T) {
	if got, want := expandHome("~/state.db", "/srv/agent"), "/srv/agent/state.db"; got != want {
		t.Errorf("expandHome(~/state.db) = %q, want %q", got, want)
	}
	if got, want := expandHome("~", "/srv/agent"), "/srv/agent"; got != want {
		t.Errorf("expandHome(~) = %q, want %q", got, want)
	}
	if got, want := expandHome("/var/lib/agent/state.db", "/srv/agent"), "/var/lib/agent/state.db"; got != want {
		t.Errorf("an absolute path must pass through untouched: got %q, want %q", got, want)
	}
}

func TestRemoveDirRemovesOnlyThatDirectory(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "work")
	if err := os.MkdirAll(filepath.Join(target, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(root, "other-stuff")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}

	removeDir(target, func(string, ...any) {})

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be removed, stat err = %v", target, err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("unrelated sibling directory must survive, stat err = %v", err)
	}
}

func TestRemoveDirIsANoOpWhenAbsent(t *testing.T) {
	target := filepath.Join(t.TempDir(), "does-not-exist")
	removeDir(target, func(string, ...any) { t.Error("should not log anything when there's nothing to remove") })
}

func TestRemoveFileRemovesOnlyThatFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "state.db")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(sibling, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	removeFile(target, func(string, ...any) {})

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be removed, stat err = %v", target, err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("unrelated sibling file must survive, stat err = %v", err)
	}
}

func TestLoadStatePathsPrefersInstalledConfigOverFallback(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "installed-config.json")
	fallback := filepath.Join(dir, "fallback-config.json")
	if err := os.WriteFile(installed, []byte(`{"workspace":{"root":"/data/work"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fallback, []byte(`{"workspace":{"root":"/should-not-be-used"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	restore := setInstalledConfigPathForTest(installed)
	defer restore()

	paths := loadStatePaths(fallback, func(string, ...any) {})
	if paths.Workspace.Root != "/data/work" {
		t.Fatalf("Workspace.Root = %q, want the installed config's value", paths.Workspace.Root)
	}
}

func TestLoadStatePathsFallsBackWhenInstalledConfigMissing(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "does-not-exist.json")
	fallback := filepath.Join(dir, "fallback-config.json")
	if err := os.WriteFile(fallback, []byte(`{"workspace":{"root":"/data/work"},"store":{"path":"/data/state.db"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	restore := setInstalledConfigPathForTest(installed)
	defer restore()

	paths := loadStatePaths(fallback, func(string, ...any) {})
	if paths.Workspace.Root != "/data/work" {
		t.Fatalf("Workspace.Root = %q, want the fallback config's value", paths.Workspace.Root)
	}
	if paths.Store.Path != "/data/state.db" {
		t.Fatalf("Store.Path = %q, want the fallback config's value", paths.Store.Path)
	}
	// Fields the fallback config didn't set keep their compiled-in defaults.
	if paths.Workspace.LogsRoot != "~/.agent-loop/logs" {
		t.Fatalf("Workspace.LogsRoot = %q, want the compiled default", paths.Workspace.LogsRoot)
	}
}

func TestLoadStatePathsFallsBackToDefaultsWhenNeitherConfigExists(t *testing.T) {
	dir := t.TempDir()
	restore := setInstalledConfigPathForTest(filepath.Join(dir, "nope.json"))
	defer restore()

	paths := loadStatePaths(filepath.Join(dir, "also-nope.json"), func(string, ...any) {})
	if paths != defaultStatePaths() {
		t.Fatalf("paths = %+v, want compiled defaults %+v", paths, defaultStatePaths())
	}
}

// setInstalledConfigPathForTest overrides the package-level installedConfigPath
// for the duration of a test and returns a func to restore it.
func setInstalledConfigPathForTest(path string) func() {
	prev := installedConfigPath
	installedConfigPath = path
	return func() { installedConfigPath = prev }
}
