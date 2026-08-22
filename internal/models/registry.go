// Package models loads models.json and turns it into an ordered fallback
// ladder for a given phase of work.
//
// No model identifier is hardcoded anywhere in the Go source: models.json is
// the single place to update when Anthropic's lineup changes.
package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	embedded "github.com/ableinc/coding-agent-loop"
)

// Roles a model may be used for.
const (
	RoleTriage    = "triage"
	RolePlan      = "plan"
	RoleImplement = "implement"
)

// Model is one entry in models.json.
type Model struct {
	// ID is the full Anthropic model identifier, recorded in run history so
	// the record stays meaningful after alias meanings shift.
	ID string `json:"id"`
	// Alias is what gets passed to `claude --model`. The CLI accepts both
	// aliases ("opus") and full IDs; aliases track the latest model.
	Alias string `json:"alias"`
	// Roles this model may serve. Empty means all roles.
	Roles []string `json:"roles"`
	// Priority orders the ladder, lowest first.
	Priority int `json:"priority"`
}

// Ref is what gets passed to the CLI: the alias when set, else the ID.
func (m Model) Ref() string {
	if m.Alias != "" {
		return m.Alias
	}
	return m.ID
}

// ServesRole reports whether m may be used for role.
func (m Model) ServesRole(role string) bool {
	if len(m.Roles) == 0 {
		return true
	}
	for _, r := range m.Roles {
		if strings.EqualFold(r, role) {
			return true
		}
	}
	return false
}

// Registry is the parsed models.json.
type Registry struct {
	Models []Model `json:"models"`
}

// Load reads and validates models.json at path. When allowEmbeddedFallback is
// true and path does not exist, the repo-root models.json embedded in the
// binary at build time (embedded.Models) is used instead, so a binary shipped
// without the rest of the repo still has a working model ladder out of the
// box. allowEmbeddedFallback should be false whenever path was explicitly
// requested (e.g. a non-default models_path, or a --models-style argument):
// an explicit path that does not exist is a misconfiguration to report, not
// something to silently paper over.
func Load(path string, allowEmbeddedFallback bool) (*Registry, error) {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist) && allowEmbeddedFallback:
		data = embedded.Models
	case err != nil:
		return nil, fmt.Errorf("read models file %s: %w", path, err)
	}
	var reg Registry
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&reg); err != nil {
		return nil, fmt.Errorf("parse models file %s: %w", path, err)
	}
	if err := reg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid models file %s: %w", path, err)
	}
	return &reg, nil
}

// Validate rejects registries that could not produce a usable ladder.
func (r *Registry) Validate() error {
	if len(r.Models) == 0 {
		return fmt.Errorf("no models defined")
	}
	seen := make(map[string]bool, len(r.Models))
	for i, m := range r.Models {
		if m.ID == "" {
			return fmt.Errorf("models[%d]: id must be set", i)
		}
		if seen[m.ID] {
			return fmt.Errorf("models[%d]: duplicate id %q", i, m.ID)
		}
		seen[m.ID] = true
		for _, role := range m.Roles {
			switch strings.ToLower(role) {
			case RoleTriage, RolePlan, RoleImplement:
			default:
				return fmt.Errorf("models[%d] (%s): unknown role %q (want %q, %q, or %q)",
					i, m.ID, role, RoleTriage, RolePlan, RoleImplement)
			}
		}
	}
	if len(r.forRole(RoleImplement)) == 0 {
		return fmt.Errorf("no model serves the %q role, so no work could ever run", RoleImplement)
	}
	if len(r.forRole(RolePlan)) == 0 {
		return fmt.Errorf("no model serves the %q role, so no plan could ever be produced", RolePlan)
	}
	return nil
}

func (r *Registry) forRole(role string) []Model {
	var out []Model
	for _, m := range r.Models {
		if m.ServesRole(role) {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out
}

// Ladder returns the priority-ordered models for role, dropping any whose ID
// is in cooledDown. If every candidate is cooled down it returns the full
// ladder anyway: refusing to run at all is worse than retrying a model whose
// cooldown may be stale, and the usage gate is the real brake.
func (r *Registry) Ladder(role string, cooledDown map[string]bool) []Model {
	all := r.forRole(role)
	if len(cooledDown) == 0 {
		return all
	}
	available := make([]Model, 0, len(all))
	for _, m := range all {
		if !cooledDown[m.ID] {
			available = append(available, m)
		}
	}
	if len(available) == 0 {
		return all
	}
	return available
}

// Head returns the first model of the ladder plus the comma-separated refs of
// the rest, ready for `--model` and `--fallback-model` respectively.
// The fallback string is empty when the ladder has a single entry.
func Head(ladder []Model) (head Model, fallbacks string, err error) {
	if len(ladder) == 0 {
		return Model{}, "", fmt.Errorf("empty model ladder")
	}
	refs := make([]string, 0, len(ladder)-1)
	for _, m := range ladder[1:] {
		refs = append(refs, m.Ref())
	}
	return ladder[0], strings.Join(refs, ","), nil
}

// Resolve maps a model identifier reported by the CLI back to a registry
// entry. The CLI reports dated IDs (claude-haiku-4-5-20251001) and canonical
// ones (claude-haiku-4-5), so match on prefix in both directions.
func (r *Registry) Resolve(reported string) (Model, bool) {
	if reported == "" {
		return Model{}, false
	}
	for _, m := range r.Models {
		if m.ID == reported || m.Alias == reported {
			return m, true
		}
	}
	for _, m := range r.Models {
		if strings.HasPrefix(reported, m.ID) || strings.HasPrefix(m.ID, reported) {
			return m, true
		}
	}
	return Model{}, false
}
