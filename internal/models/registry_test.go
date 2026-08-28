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
	{"id":"claude-opus-5","alias":"opus","roles":["plan","implement"],"priority":1,"effort":{"plan":"high","implement":"medium"}},
	{"id":"claude-sonnet-5","alias":"sonnet","roles":["plan","implement"],"priority":2,"effort":{"plan":"high","implement":"medium"}},
	{"id":"claude-haiku-4-5","alias":"haiku","roles":["implement"],"priority":3}
]}`

func TestLoadAndLadderOrder(t *testing.T) {
	reg, err := Load(writeModels(t, goodModels), false)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	ladder := reg.Ladder(RoleImplement, nil)
	if len(ladder) != 3 {
		t.Fatalf("expected 3 implement models, got %d", len(ladder))
	}
	if ladder[0].ID != "claude-opus-5" || ladder[1].ID != "claude-sonnet-5" || ladder[2].ID != "claude-haiku-4-5" {
		t.Fatalf("ladder not ordered by priority: %+v", ladder)
	}

	head, fallbacks, err := Head(ladder)
	if err != nil {
		t.Fatal(err)
	}
	if head.Ref() != "opus" {
		t.Fatalf("head ref = %q, want opus", head.Ref())
	}
	if fallbacks != "sonnet,haiku" {
		t.Fatalf("fallbacks = %q, want sonnet,haiku", fallbacks)
	}

	if e := head.EffortFor(RoleImplement); e != "medium" {
		t.Fatalf("opus implement effort = %q, want medium", e)
	}
	if e := head.EffortFor(RolePlan); e != "high" {
		t.Fatalf("opus plan effort = %q, want high", e)
	}
	if e := ladder[2].EffortFor(RoleImplement); e != "" {
		t.Fatalf("haiku has no configured effort, want \"\", got %q", e)
	}

	plan := reg.Ladder(RolePlan, nil)
	if len(plan) != 2 || plan[0].ID != "claude-opus-5" || plan[1].ID != "claude-sonnet-5" {
		t.Fatalf("plan ladder wrong: %+v", plan)
	}
}

func TestLadderSkipsCooledDownModels(t *testing.T) {
	reg, err := Load(writeModels(t, goodModels), false)
	if err != nil {
		t.Fatal(err)
	}
	ladder := reg.Ladder(RoleImplement, map[string]bool{"claude-opus-5": true})
	if len(ladder) != 2 || ladder[0].ID != "claude-sonnet-5" || ladder[1].ID != "claude-haiku-4-5" {
		t.Fatalf("cooled model should be skipped: %+v", ladder)
	}
}

// Refusing to run at all is worse than retrying a model whose cooldown may be
// stale — the usage gate is the real brake.
func TestLadderFallsBackWhenEverythingIsCooledDown(t *testing.T) {
	reg, err := Load(writeModels(t, goodModels), false)
	if err != nil {
		t.Fatal(err)
	}
	cooled := map[string]bool{"claude-opus-5": true, "claude-sonnet-5": true, "claude-haiku-4-5": true}
	if ladder := reg.Ladder(RoleImplement, cooled); len(ladder) != 3 {
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
	if !m.ServesRole(RoleImplement) || !m.ServesRole(RolePlan) {
		t.Fatal("a model with no roles should serve all of them")
	}
}

func TestRefFallsBackToID(t *testing.T) {
	if got := (Model{ID: "claude-opus-5"}).Ref(); got != "claude-opus-5" {
		t.Fatalf("Ref() = %q, want the id when no alias is set", got)
	}
}

func TestResolveMatchesDatedAndCanonicalIDs(t *testing.T) {
	reg, err := Load(writeModels(t, goodModels), false)
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

// A binary shipped without the rest of the repo has nothing at the default
// path, so Load must fall back to the ladder embedded at build time rather
// than failing outright.
func TestLoadFallsBackToEmbeddedDefaultWhenFileMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.json")
	reg, err := Load(missing, true)
	if err != nil {
		t.Fatalf("load should fall back to the embedded default, got: %v", err)
	}
	if len(reg.Ladder(RoleImplement, nil)) == 0 {
		t.Fatal("embedded default should produce a usable implement ladder")
	}
}

// An explicitly requested path (allowEmbeddedFallback=false) that doesn't
// exist is a misconfiguration to report, not something to paper over.
func TestLoadRejectsMissingExplicitPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.json")
	if _, err := Load(missing, false); err == nil {
		t.Fatal("an explicit path that does not exist must be an error, not a silent embedded fallback")
	}
}

// A file present at path is the whole point of letting an operator customize
// the ladder without rebuilding — it must win over the embedded default.
func TestLoadPrefersFileOverEmbeddedDefault(t *testing.T) {
	reg, err := Load(writeModels(t, `{"models":[{"id":"custom-model","alias":"custom","roles":["plan","implement"],"priority":1}]}`), true)
	if err != nil {
		t.Fatal(err)
	}
	ladder := reg.Ladder(RoleImplement, nil)
	if len(ladder) != 1 || ladder[0].ID != "custom-model" {
		t.Fatalf("file on disk should override the embedded default, got %+v", ladder)
	}
}

func TestValidationRejectsBadRegistries(t *testing.T) {
	tests := map[string]string{
		"no models":               `{"models":[]}`,
		"missing id":              `{"models":[{"alias":"x","roles":["implement"]}]}`,
		"duplicate id":            `{"models":[{"id":"a","roles":["implement"]},{"id":"a","roles":["implement"]}]}`,
		"unknown role":            `{"models":[{"id":"a","roles":["deploy"]}]}`,
		"no implement role":       `{"models":[{"id":"a","roles":["plan"]}]}`,
		"no plan role":            `{"models":[{"id":"a","roles":["implement"]}]}`,
		"unknown field":           `{"models":[{"id":"a"}],"extra":true}`,
		"unknown effort":          `{"models":[{"id":"a","roles":["plan","implement"],"effort":{"plan":"extreme"}}]}`,
		"effort for unknown role": `{"models":[{"id":"a","roles":["plan","implement"],"effort":{"deploy":"high"}}]}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeModels(t, body), false); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}
