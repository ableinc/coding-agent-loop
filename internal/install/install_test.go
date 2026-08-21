package install

import (
	"os"
	"os/user"
	"strconv"
	"strings"
	"testing"
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
	if !strings.Contains(unit, "ReadWritePaths=/opt/coding-agent-loop /home/alice/.agent-loop") {
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
