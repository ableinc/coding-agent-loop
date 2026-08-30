// Package gate decides whether the loop may claim new work.
//
// Per the chosen policy this is limit-aware only: the loop runs around the
// clock and stops for exactly two reasons — a real usage limit reported by the
// Claude CLI, or an operator pause. There is no time-of-day schedule.
//
// Two sources of signal, deliberately unequal:
//
//   - The CLI itself, which is authoritative. It is the channel that actually
//     failed, so a limit seen there closes the gate.
//   - The OAuth usage endpoint, which is advisory only. It rate-limits hard
//     (the machine's statusline cache shows 429 backoff in normal use) and its
//     payload shape is not contractual, so it populates /status and never
//     closes the gate on its own.
package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ableinc/coding-agent-loop/internal/claude"
	"github.com/ableinc/coding-agent-loop/internal/config"
	"github.com/ableinc/coding-agent-loop/internal/store"
)

const usageURL = "https://api.anthropic.com/api/oauth/usage"

// backoffLadder is used when a limit is reported without a reset time.
var backoffLadder = []time.Duration{5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour}

// LimitHit describes a detected usage limit.
type LimitHit struct {
	// ResetAt is when the limit lifts, when the CLI told us.
	ResetAt time.Time
	// HasReset is false when we had to fall back to a backoff.
	HasReset bool
	// Reason is a short human-readable explanation for /status.
	Reason string
}

var (
	// "Claude AI usage limit reached|1786679747"
	epochResetRe = regexp.MustCompile(`usage limit reached\s*\|\s*(\d{9,})`)
	// Any bare epoch following a reset phrase.
	resetEpochRe = regexp.MustCompile(`(?i)reset[^0-9]{0,20}(\d{9,})`)
	limitPhrases = []string{
		"usage limit reached",
		"usage limit has been reached",
		"rate limit exceeded",
		"rate_limit_error",
		"too many requests",
		"you've reached your usage limit",
		"upgrade to increase your usage limit",
	}
	// authExpiredPhrases catch a hard-expired OAuth token. Unlike a usage
	// limit this cannot self-heal on a timer: the CLI needs to persist a
	// refreshed token (or an operator needs to re-run `claude login`), so it
	// is classified and gated separately — see DetectAuthExpired.
	authExpiredPhrases = []string{
		"oauth access token has expired",
		"re-authenticate to continue",
	}
)

// DetectLimit classifies a failed run. It is pure so it can be tested against
// captured CLI output.
func DetectLimit(res *claude.Result, runErr error) (LimitHit, bool) {
	var parts []string
	if res != nil {
		parts = append(parts, res.Result, res.Stderr, res.Subtype, res.TerminalReason)
		if res.APIErrorStatus != nil && *res.APIErrorStatus == http.StatusTooManyRequests {
			return LimitHit{Reason: "claude reported HTTP 429"}, true
		}
	}
	if runErr != nil {
		parts = append(parts, runErr.Error())
	}
	haystack := strings.ToLower(strings.Join(parts, "\n"))
	if strings.TrimSpace(haystack) == "" {
		return LimitHit{}, false
	}

	matched := ""
	for _, p := range limitPhrases {
		if strings.Contains(haystack, p) {
			matched = p
			break
		}
	}
	if matched == "" {
		return LimitHit{}, false
	}

	hit := LimitHit{Reason: "claude reported: " + matched}
	for _, re := range []*regexp.Regexp{epochResetRe, resetEpochRe} {
		if m := re.FindStringSubmatch(haystack); len(m) == 2 {
			if secs, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				reset := time.Unix(secs, 0)
				// Guard against a nonsense timestamp locking the loop out for
				// years, or one already in the past.
				if reset.After(time.Now()) && reset.Before(time.Now().Add(24*time.Hour)) {
					hit.ResetAt = reset
					hit.HasReset = true
					hit.Reason = fmt.Sprintf("%s (resets %s)", hit.Reason, reset.Format(time.RFC3339))
				}
			}
			break
		}
	}
	return hit, true
}

// AuthExpiredHit describes a detected hard OAuth token expiry.
type AuthExpiredHit struct {
	// Reason is a short human-readable explanation for /status.
	Reason string
}

// DetectAuthExpired classifies a failed run as a hard OAuth expiry, distinct
// from DetectLimit's usage limits: this cannot resolve itself on a timer, so
// it must not be folded into LimitHit's backoff-and-retry semantics.
func DetectAuthExpired(res *claude.Result, runErr error) (AuthExpiredHit, bool) {
	var parts []string
	if res != nil {
		parts = append(parts, res.Result, res.Stderr, res.Subtype, res.TerminalReason)
	}
	if runErr != nil {
		parts = append(parts, runErr.Error())
	}
	haystack := strings.ToLower(strings.Join(parts, "\n"))
	if strings.TrimSpace(haystack) == "" {
		return AuthExpiredHit{}, false
	}
	for _, p := range authExpiredPhrases {
		if strings.Contains(haystack, p) {
			return AuthExpiredHit{
				Reason: "OAuth token expired — check that ~/.claude/.credentials.json is writable by the service account so the CLI can persist its refreshed token",
			}, true
		}
	}
	return AuthExpiredHit{}, false
}

// Snapshot is the advisory usage picture shown on /status.
type Snapshot struct {
	FetchedAt   time.Time `json:"fetched_at"`
	Available   bool      `json:"available"`
	UsedUSD     float64   `json:"used_usd,omitempty"`
	LimitUSD    float64   `json:"limit_usd,omitempty"`
	Percent     float64   `json:"percent,omitempty"`
	Enabled     bool      `json:"enabled"`
	Note        string    `json:"note,omitempty"`
	CooldownEnd time.Time `json:"cooldown_end"`
}

// Gate evaluates and records gating state.
type Gate struct {
	store *store.Store
	cfg   config.ClaudeConfig
	log   func(format string, args ...any)

	mu          sync.Mutex
	backoffStep int
	snapshot    Snapshot
	lastPoll    time.Time
	cooldownEnd time.Time
	client      *http.Client
}

// New builds a Gate.
func New(st *store.Store, cfg config.ClaudeConfig, log func(string, ...any)) *Gate {
	return &Gate{
		store:  st,
		cfg:    cfg,
		log:    log,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (g *Gate) logf(format string, args ...any) {
	if g.log != nil {
		g.log(format, args...)
	}
}

// Status is the full gating picture.
type Status struct {
	Allowed bool         `json:"allowed"`
	Gates   []store.Gate `json:"gates"`
	Usage   Snapshot     `json:"usage"`
}

// Check reports whether new work may be claimed and why not, if not.
func (g *Gate) Check(ctx context.Context) (Status, error) {
	gates, err := g.store.ActiveGates(ctx)
	if err != nil {
		return Status{}, err
	}
	blocking := make([]store.Gate, 0, len(gates))
	for _, gt := range gates {
		// Model cooldowns steer the ladder; they do not stop the loop.
		if strings.HasPrefix(gt.Kind, store.GateModelPrefix) {
			continue
		}
		blocking = append(blocking, gt)
	}
	return Status{Allowed: len(blocking) == 0, Gates: gates, Usage: g.Snapshot()}, nil
}

// RecordLimit closes the usage gate. When the CLI gave no reset time it walks
// a backoff ladder so repeated limits do not turn into a hot retry loop.
func (g *Gate) RecordLimit(ctx context.Context, hit LimitHit) (time.Time, error) {
	until := hit.ResetAt
	if !hit.HasReset {
		g.mu.Lock()
		step := g.backoffStep
		if step >= len(backoffLadder) {
			step = len(backoffLadder) - 1
		}
		g.backoffStep++
		g.mu.Unlock()
		until = time.Now().Add(backoffLadder[step])
	}
	// Add a small cushion so we do not retry the instant the window opens.
	until = until.Add(30 * time.Second)

	if err := g.store.SetGate(ctx, store.GateUsageLimit, until, hit.Reason); err != nil {
		return time.Time{}, err
	}
	g.logf("usage gate closed until %s: %s", until.Format(time.RFC3339), hit.Reason)
	return until, nil
}

// RecordAuthExpired closes the gate indefinitely on a hard OAuth expiry: it
// cannot self-heal on a timer the way a usage limit does, so — like Pause —
// it stays closed until an operator fixes the credentials and calls Resume.
func (g *Gate) RecordAuthExpired(ctx context.Context, hit AuthExpiredHit) (time.Time, error) {
	until := time.Now().Add(100 * 365 * 24 * time.Hour)
	if err := g.store.SetGate(ctx, store.GateAuthExpired, until, hit.Reason); err != nil {
		return time.Time{}, err
	}
	return until, nil
}

// RecordSuccess reopens the usage gate after a run completes normally.
func (g *Gate) RecordSuccess(ctx context.Context) error {
	g.mu.Lock()
	g.backoffStep = 0
	g.mu.Unlock()
	return g.store.ClearGate(ctx, store.GateUsageLimit)
}

// CoolDownModel sidelines one model without stopping the loop, so the next run
// starts lower on the ladder instead of re-hitting the same wall.
func (g *Gate) CoolDownModel(ctx context.Context, modelID string, d time.Duration, reason string) error {
	if modelID == "" {
		return nil
	}
	return g.store.SetGate(ctx, store.GateModelPrefix+modelID, time.Now().Add(d), reason)
}

// Pause and Resume are the operator switches behind POST /pause and /resume.
func (g *Gate) Pause(ctx context.Context, reason string) error {
	if reason == "" {
		reason = "paused by operator"
	}
	// Far enough out to be effectively indefinite; Resume is the way back.
	return g.store.SetGate(ctx, store.GatePause, time.Now().Add(100*365*24*time.Hour), reason)
}

func (g *Gate) Resume(ctx context.Context) error {
	return g.store.ClearGate(ctx, store.GatePause)
}

// Snapshot returns the last advisory usage reading.
func (g *Gate) Snapshot() Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.snapshot
	s.CooldownEnd = g.cooldownEnd
	return s
}

// --- advisory usage polling -------------------------------------------------

type usagePayload struct {
	Spend struct {
		Used struct {
			AmountMinor float64 `json:"amount_minor"`
			Exponent    *int    `json:"exponent"`
		} `json:"used"`
		Limit struct {
			AmountMinor float64 `json:"amount_minor"`
			Exponent    *int    `json:"exponent"`
		} `json:"limit"`
		Percent float64 `json:"percent"`
		Enabled *bool   `json:"enabled"`
	} `json:"spend"`
}

type usageCache struct {
	FetchedAt     int64    `json:"fetched_at"`
	CooldownUntil int64    `json:"cooldown_until"`
	Snapshot      Snapshot `json:"snapshot"`
}

// PollUsage refreshes the advisory snapshot, respecting the throttle and any
// 429 cooldown. It never returns an error that should stop the loop: a failure
// here means "no signal", not "blocked".
func (g *Gate) PollUsage(ctx context.Context) {
	g.mu.Lock()
	if time.Now().Before(g.cooldownEnd) || time.Since(g.lastPoll) < g.cfg.UsagePollInterval.D() {
		g.mu.Unlock()
		return
	}
	g.lastPoll = time.Now()
	g.mu.Unlock()

	snap, cooldown, err := g.fetchUsage(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	if cooldown > 0 {
		g.cooldownEnd = time.Now().Add(cooldown)
	}
	if err != nil {
		g.snapshot = Snapshot{FetchedAt: time.Now(), Available: false, Note: err.Error()}
		g.logf("usage poll unavailable (advisory only, loop unaffected): %v", err)
	} else {
		g.snapshot = snap
	}
	g.writeCache()
}

func (g *Gate) writeCache() {
	path := g.cfg.UsageCachePath
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.Marshal(usageCache{
		FetchedAt:     time.Now().Unix(),
		CooldownUntil: g.cooldownEnd.Unix(),
		Snapshot:      g.snapshot,
	})
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o600)
}

func (g *Gate) fetchUsage(ctx context.Context) (Snapshot, time.Duration, error) {
	token, err := readOAuthToken(g.cfg.CredentialsPath)
	if err != nil {
		return Snapshot{}, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return Snapshot{}, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return Snapshot{}, 0, fmt.Errorf("usage request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return Snapshot{}, g.cfg.UsageBackoff.D(), fmt.Errorf("usage endpoint rate-limited (429)")
	}
	if resp.StatusCode != http.StatusOK {
		return Snapshot{}, 0, fmt.Errorf("usage endpoint returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Snapshot{}, 0, fmt.Errorf("read usage response: %w", err)
	}
	var payload usagePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return Snapshot{}, 0, fmt.Errorf("decode usage response: %w", err)
	}

	snap := Snapshot{FetchedAt: time.Now(), Available: true, Enabled: true}
	if payload.Spend.Enabled != nil {
		snap.Enabled = *payload.Spend.Enabled
	}
	snap.UsedUSD = minorToUnits(payload.Spend.Used.AmountMinor, payload.Spend.Used.Exponent)
	snap.LimitUSD = minorToUnits(payload.Spend.Limit.AmountMinor, payload.Spend.Limit.Exponent)
	snap.Percent = payload.Spend.Percent
	if snap.LimitUSD == 0 && snap.UsedUSD == 0 {
		snap.Note = "plan does not expose spend figures"
	}
	return snap, 0, nil
}

func minorToUnits(amount float64, exponent *int) float64 {
	exp := 2
	if exponent != nil {
		exp = *exponent
	}
	div := 1.0
	for i := 0; i < exp; i++ {
		div *= 10
	}
	return amount / div
}

func readOAuthToken(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("no credentials path configured")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read claude credentials: %w", err)
	}
	var creds struct {
		ClaudeAIOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", fmt.Errorf("parse claude credentials: %w", err)
	}
	if creds.ClaudeAIOauth.AccessToken == "" {
		return "", fmt.Errorf("no OAuth token in %s (subscription login required)", path)
	}
	return creds.ClaudeAIOauth.AccessToken, nil
}
