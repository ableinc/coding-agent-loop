package install

import (
	"os"
	"strings"
	"testing"
)

// The embed directive is the whole point of this package — if it silently
// picked up an empty file the binary would install a no-op unit.
func TestServiceUnitEmbedded(t *testing.T) {
	if len(ServiceUnit) == 0 {
		t.Fatal("embedded service unit is empty")
	}
	got := string(ServiceUnit)
	for _, want := range []string{"[Unit]", "[Service]", "[Install]", "ExecStart=/opt/agent-loop/bin/agent-loop"} {
		if !strings.Contains(got, want) {
			t.Errorf("embedded unit missing %q", want)
		}
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
