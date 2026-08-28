package config

import (
	"slices"
	"testing"
)

func TestMigratePreservesUserValues(t *testing.T) {
	old := `{"github":{"owners":["acme"],"poll_interval":"2m"},"discord":{"enabled":true,"webhook_url":"https://example.com/hook"}}`
	result, err := Migrate([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Config.GitHub.Owners; len(got) != 1 || got[0] != "acme" {
		t.Fatalf("owners = %v, want [acme]", got)
	}
	if result.Config.GitHub.PollInterval.D().String() != "2m0s" {
		t.Fatalf("poll_interval = %v, want 2m0s", result.Config.GitHub.PollInterval.D())
	}
	if !result.Config.Discord.Enabled || result.Config.Discord.WebhookURL != "https://example.com/hook" {
		t.Fatalf("discord config not preserved: %+v", result.Config.Discord)
	}
}

func TestMigrateFillsInNewFieldsAtDefault(t *testing.T) {
	// A field the schema now has (server.ui) that the old file never set.
	old := `{"github":{"owners":["acme"]},"server":{"addr":"127.0.0.1:9999"}}`
	result, err := Migrate([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	if result.Config.Server.Addr != "127.0.0.1:9999" {
		t.Fatalf("server.addr = %q, want the user's own value preserved", result.Config.Server.Addr)
	}
	if result.Config.Server.UI != Default().Server.UI {
		t.Fatalf("server.ui = %v, want the default %v since the old file never set it", result.Config.Server.UI, Default().Server.UI)
	}
	if !slices.Contains(result.Added, "server.ui") {
		t.Fatalf("Added = %v, want it to include server.ui", result.Added)
	}
}

func TestMigrateReportsDroppedFields(t *testing.T) {
	old := `{"github":{"owners":["acme"],"no_longer_a_field":true}}`
	result, err := Migrate([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(result.Dropped, "github.no_longer_a_field") {
		t.Fatalf("Dropped = %v, want it to include github.no_longer_a_field", result.Dropped)
	}
}

func TestMigrateRejectsInvalidJSON(t *testing.T) {
	if _, err := Migrate([]byte("not json")); err == nil {
		t.Fatal("expected an error for invalid JSON")
	}
}

func TestMigrateDoesNotExpandPaths(t *testing.T) {
	old := `{"github":{"owners":["acme"]},"workspace":{"root":"~/.agent-loop/work"}}`
	result, err := Migrate([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	if result.Config.Workspace.Root != "~/.agent-loop/work" {
		t.Fatalf("workspace.root = %q, want the tilde left unexpanded", result.Config.Workspace.Root)
	}
}
