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
