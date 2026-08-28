package web

import (
	"io/fs"
	"strings"
	"testing"
)

// TestAssetsPresent guards against a rename that would silently ship a
// console with a missing script or stylesheet: go:embed only fails the
// build if the whole "assets" directory is missing, not if one file inside
// it is renamed out from under index.html's references.
func TestAssetsPresent(t *testing.T) {
	for _, name := range []string{"index.html", "app.css", "app.js"} {
		if _, err := fs.Stat(Assets, name); err != nil {
			t.Fatalf("expected asset %q to exist: %v", name, err)
		}
	}
}

func TestIndexReferencesOtherAssets(t *testing.T) {
	data, err := fs.ReadFile(Assets, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)
	for _, ref := range []string{"app.css", "app.js"} {
		if !strings.Contains(html, ref) {
			t.Errorf("index.html does not reference %q", ref)
		}
	}
}

// TestLockScreenPresent guards the password-lock UI (issue #13): the lock
// screen markup must exist for auth.go's server.password gate to have
// anything to show, and the session token must live in sessionStorage, not
// localStorage, or "closing the tab logs you out" silently stops being true.
func TestLockScreenPresent(t *testing.T) {
	html, err := fs.ReadFile(Assets, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), "lock-screen") {
		t.Error("index.html does not contain the lock-screen element")
	}

	js, err := fs.ReadFile(Assets, "app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), "sessionStorage") {
		t.Error("app.js does not use sessionStorage for the session token")
	}
}
