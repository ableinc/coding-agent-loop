package models

import (
	"os"
	"path/filepath"
	"testing"
)

func writeModels(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const goodModels = `{"models":[
	{"id":"claude-opus-5","alias":"opus","roles":["implement"],"priority":1},
	{"id":"claude-sonnet-5","alias":"sonnet","roles":["implement"],"priority":2},
	{"id":"claude-haiku-4-5","alias":"haiku","roles":["triage"],"priority":1}
]}`

func TestLoadAndLadderOrder(t *testing.T) {
	reg, err := Load(writeModels(t, goodModels))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	ladder := reg.Ladder(RoleImplement, nil)
	if len(ladder) != 2 {
		t.Fatalf("expected 2 implement models, got %d", len(ladder))
	}
	if ladder[0].ID != "claude-opus-5" || ladder[1].ID != "claude-sonnet-5" {
		t.Fatalf("ladder not ordered by priority: %+v", ladder)
	}

	head, fallbacks, err := Head(ladder)
	if err != nil {
		t.Fatal(err)
	}
	if head.Ref() != "opus" {
		t.Fatalf("head ref = %q, want opus", head.Ref())
	}
	if fallbacks != "sonnet" {
		t.Fatalf("fallbacks = %q, want sonnet", fallbacks)
	}

	if triage := reg.Ladder(RoleTriage, nil); len(triage) != 1 || triage[0].ID != "claude-haiku-4-5" {
		t.Fatalf("triage ladder wrong: %+v", triage)
	}
}

func TestLadderSkipsCooledDownModels(t *testing.T) {
	reg, err := Load(writeModels(t, goodModels))
	if err != nil {
		t.Fatal(err)
	}
	ladder := reg.Ladder(RoleImplement, map[string]bool{"claude-opus-5": true})
	if len(ladder) != 1 || ladder[0].ID != "claude-sonnet-5" {
		t.Fatalf("cooled model should be skipped: %+v", ladder)
	}
}

// Refusing to run at all is worse than retrying a model whose cooldown may be
// stale — the usage gate is the real brake.
func TestLadderFallsBackWhenEverythingIsCooledDown(t *testing.T) {
	reg, err := Load(writeModels(t, goodModels))
	if err != nil {
		t.Fatal(err)
	}
	cooled := map[string]bool{"claude-opus-5": true, "claude-sonnet-5": true}
	if ladder := reg.Ladder(RoleImplement, cooled); len(ladder) != 2 {
		t.Fatalf("expected the full ladder as a fallback, got %+v", ladder)
	}
}

func TestHeadOnSingleEntryHasNoFallbacks(t *testing.T) {
	_, fallbacks, err := Head([]Model{{ID: "only", Alias: "only"}})
	if err != nil {
		t.Fatal(err)
	}
	if fallbacks != "" {
		t.Fatalf("single-entry ladder should produce no --fallback-model, got %q", fallbacks)
	}
	if _, _, err := Head(nil); err == nil {
		t.Fatal("empty ladder must be an error")
	}
}

func TestModelWithNoRolesServesEverything(t *testing.T) {
	m := Model{ID: "x"}
	if !m.ServesRole(RoleImplement) || !m.ServesRole(RoleTriage) {
		t.Fatal("a model with no roles should serve all of them")
	}
}

func TestRefFallsBackToID(t *testing.T) {
	if got := (Model{ID: "claude-opus-5"}).Ref(); got != "claude-opus-5" {
		t.Fatalf("Ref() = %q, want the id when no alias is set", got)
	}
}

func TestResolveMatchesDatedAndCanonicalIDs(t *testing.T) {
	reg, err := Load(writeModels(t, goodModels))
	if err != nil {
		t.Fatal(err)
	}
	// The CLI reports dated IDs in modelUsage.
	if m, ok := reg.Resolve("claude-haiku-4-5-20251001"); !ok || m.ID != "claude-haiku-4-5" {
		t.Fatalf("dated id should resolve: %+v ok=%v", m, ok)
	}
	if m, ok := reg.Resolve("opus"); !ok || m.ID != "claude-opus-5" {
		t.Fatalf("alias should resolve: %+v ok=%v", m, ok)
	}
	if _, ok := reg.Resolve("gpt-9"); ok {
		t.Fatal("unknown model must not resolve")
	}
	if _, ok := reg.Resolve(""); ok {
		t.Fatal("empty model must not resolve")
	}
}

func TestValidationRejectsBadRegistries(t *testing.T) {
	tests := map[string]string{
		"no models":         `{"models":[]}`,
		"missing id":        `{"models":[{"alias":"x","roles":["implement"]}]}`,
		"duplicate id":      `{"models":[{"id":"a","roles":["implement"]},{"id":"a","roles":["implement"]}]}`,
		"unknown role":      `{"models":[{"id":"a","roles":["deploy"]}]}`,
		"no implement role": `{"models":[{"id":"a","roles":["triage"]}]}`,
		"unknown field":     `{"models":[{"id":"a"}],"extra":true}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeModels(t, body)); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}
