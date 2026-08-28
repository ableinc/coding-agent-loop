package config

import (
	"encoding/json"
	"fmt"
	"sort"
)

// MigrateResult reports the outcome of merging an existing config.json into
// the current schema.
type MigrateResult struct {
	// Config is the merged result: every value the old file set explicitly is
	// kept, and every field the schema has gained since is filled in with its
	// default.
	Config Config
	// Added lists dotted field paths that exist in the current schema but
	// were absent from the old file, and therefore now carry their default.
	Added []string
	// Dropped lists dotted field paths the old file set that no longer exist
	// in the current schema. They were ignored rather than causing an error.
	Dropped []string
}

// Migrate re-expresses an existing config.json's raw bytes in the current
// schema: every non-default value the user entered is preserved, every field
// added to the schema since is filled in with its default, and any field the
// schema has since dropped is reported rather than rejected — unlike Load,
// which uses DisallowUnknownFields and would fail outright on exactly that
// file.
//
// Path fields (workspace.root, store.path, ...) are left exactly as found:
// Load's tilde-expansion is a runtime concern, not something a migration
// should bake into the file on disk.
func Migrate(raw []byte) (MigrateResult, error) {
	cfg := Default()
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return MigrateResult{}, fmt.Errorf("parse existing config: %w", err)
	}

	var oldTree map[string]any
	if err := json.Unmarshal(raw, &oldTree); err != nil {
		return MigrateResult{}, fmt.Errorf("parse existing config: %w", err)
	}
	latestRaw, err := json.Marshal(Default())
	if err != nil {
		return MigrateResult{}, fmt.Errorf("marshal current schema: %w", err)
	}
	var latestTree map[string]any
	if err := json.Unmarshal(latestRaw, &latestTree); err != nil {
		return MigrateResult{}, fmt.Errorf("unmarshal current schema: %w", err)
	}

	added := diffKeys(latestTree, oldTree, "")
	dropped := diffKeys(oldTree, latestTree, "")
	sort.Strings(added)
	sort.Strings(dropped)

	return MigrateResult{Config: cfg, Added: added, Dropped: dropped}, nil
}

// diffKeys lists the dotted paths present in a but missing from b, recursing
// into nested objects that exist on both sides so a field added or removed
// deep in the tree (e.g. "github.pr_comments.mention") is reported by its
// full path rather than just the top-level section that contains it.
func diffKeys(a, b map[string]any, prefix string) []string {
	var out []string
	for k, av := range a {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		bv, ok := b[k]
		if !ok {
			out = append(out, path)
			continue
		}
		aMap, aIsMap := av.(map[string]any)
		bMap, bIsMap := bv.(map[string]any)
		if aIsMap && bIsMap {
			out = append(out, diffKeys(aMap, bMap, path)...)
		}
	}
	return out
}
