// Package server exposes the daemon's control and status API.
//
// An autonomous process that pushes branches and opens pull requests is
// otherwise invisible, so this is how you see what it is doing and how you
// stop it. It binds to loopback by default and has no authentication: it can
// pause and cancel work, so it must not be exposed.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/static"

	"github.com/ableinc/coding-agent-loop/internal/config"
	"github.com/ableinc/coding-agent-loop/internal/discord"
	"github.com/ableinc/coding-agent-loop/internal/gate"
	"github.com/ableinc/coding-agent-loop/internal/models"
	"github.com/ableinc/coding-agent-loop/internal/store"
	"github.com/ableinc/coding-agent-loop/internal/web"
)

// Controller is the slice of the orchestrator the API needs. Keeping it an
// interface here keeps the packages independent and makes the handlers
// testable without a live loop.
type Controller interface {
	Cancel(runID string) bool
	ActiveRepos() []string
	// Poll asks for a discovery pass now, reporting false if one is already
	// queued.
	Poll() bool
}

// Options are the Server's dependencies.
type Options struct {
	Addr     string
	Store    *store.Store
	Gate     *gate.Gate
	Ctrl     Controller
	Log      *slog.Logger
	Discord  *discord.Notifier
	Config   config.Config
	Registry *models.Registry
}

// Server wraps the Fiber app.
type Server struct {
	app      *fiber.App
	addr     string
	store    *store.Store
	gate     *gate.Gate
	ctrl     Controller
	log      *slog.Logger
	start    time.Time
	discord  *discord.Notifier
	cfg      config.Config
	registry *models.Registry
}

// New builds the API.
func New(o Options) *Server {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		app:      fiber.New(fiber.Config{AppName: "coding-agent-loop"}),
		addr:     o.Addr,
		store:    o.Store,
		gate:     o.Gate,
		ctrl:     o.Ctrl,
		log:      log,
		start:    time.Now(),
		discord:  o.Discord,
		cfg:      o.Config,
		registry: o.Registry,
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.app.Use(s.sameOrigin)

	s.app.Get("/healthz", s.health)
	s.app.Get("/status", s.status)
	s.app.Get("/runs", s.listRuns)
	s.app.Get("/runs/:id", s.getRun)
	s.app.Get("/runs/:id/log", s.getRunLog)
	s.app.Get("/sessions", s.listSessions)
	s.app.Post("/pause", s.pause)
	s.app.Post("/resume", s.resume)
	s.app.Post("/runs/:id/cancel", s.cancelRun)
	s.app.Get("/config", s.getConfig)
	s.app.Get("/models", s.getModels)
	s.app.Post("/poll", s.pollNow)

	if s.cfg.Server.UI {
		s.app.Get("/", func(c fiber.Ctx) error { return c.Redirect().Status(http.StatusFound).To("/ui/") })
		s.app.Use("/ui", static.New("", static.Config{
			FS:            web.Assets,
			IndexNames:    []string{"index.html"},
			CacheDuration: -1, // no stale console after an upgrade
		}))
	}
}

// sameOrigin rejects a mutating request whose Origin (or, lacking that,
// Referer) header names a non-loopback host. A page loaded from any other
// origin can still fire a simple cross-origin POST with no preflight, and
// shipping a browser console here makes it worth closing that off. A request
// with neither header — curl, a script, an operator's own tooling — is
// unaffected: those are the documented way to drive this API and carry no
// Origin at all.
func (s *Server) sameOrigin(c fiber.Ctx) error {
	if c.Method() == fiber.MethodGet || c.Method() == fiber.MethodHead {
		return c.Next()
	}
	origin := c.Get(fiber.HeaderOrigin)
	if origin == "" {
		origin = c.Get(fiber.HeaderReferer)
	}
	if origin == "" {
		return c.Next()
	}
	if !isLoopbackOrigin(origin, s.addr) {
		return s.fail(c, http.StatusForbidden, errors.New("cross-origin request refused"))
	}
	return c.Next()
}

// isLoopbackOrigin reports whether origin (an Origin or Referer header value)
// names localhost, a loopback IP, or the host this server itself is bound to.
func isLoopbackOrigin(origin, addr string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	if addrHost, _, err := net.SplitHostPort(addr); err == nil && addrHost != "" && strings.EqualFold(addrHost, host) {
		return true
	}
	return false
}

// Listen serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Listen(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("control api listening", "addr", s.addr)
		if err := s.app.Listen(s.addr, fiber.ListenConfig{DisableStartupMessage: true}); err != nil {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := s.app.ShutdownWithContext(shutdownCtx); err != nil {
			return err
		}
		return nil
	}
}

func (s *Server) health(c fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"status":  "ok",
		"uptime":  time.Since(s.start).Round(time.Second).String(),
		"started": s.start.Format(time.RFC3339),
	})
}

type gateView struct {
	Kind         string    `json:"kind"`
	BlockedUntil time.Time `json:"blocked_until"`
	Reason       string    `json:"reason"`
	Blocking     bool      `json:"blocking"`
}

func (s *Server) status(c fiber.Ctx) error {
	ctx := c.Context()

	gs, err := s.gate.Check(ctx)
	if err != nil {
		return s.fail(c, http.StatusInternalServerError, err)
	}
	inFlight, err := s.store.InFlightRuns(ctx)
	if err != nil {
		return s.fail(c, http.StatusInternalServerError, err)
	}
	claims, err := s.store.ActiveClaims(ctx)
	if err != nil {
		return s.fail(c, http.StatusInternalServerError, err)
	}

	gates := make([]gateView, 0, len(gs.Gates))
	cooldowns := map[string]time.Time{}
	for _, g := range gs.Gates {
		isModel := len(g.Kind) > len(store.GateModelPrefix) && g.Kind[:len(store.GateModelPrefix)] == store.GateModelPrefix
		gates = append(gates, gateView{
			Kind: g.Kind, BlockedUntil: g.BlockedUntil, Reason: g.Reason, Blocking: !isModel,
		})
		if isModel {
			cooldowns[g.Kind[len(store.GateModelPrefix):]] = g.BlockedUntil
		}
	}

	var activeRepos []string
	if s.ctrl != nil {
		activeRepos = s.ctrl.ActiveRepos()
	}

	return c.JSON(fiber.Map{
		"claiming_work":   gs.Allowed,
		"gates":           gates,
		"model_cooldowns": cooldowns,
		"usage":           gs.Usage,
		"active_repos":    activeRepos,
		"claims":          claims,
		"in_flight":       inFlight,
		"uptime":          time.Since(s.start).Round(time.Second).String(),
	})
}

func (s *Server) listRuns(c fiber.Ctx) error {
	limit := fiber.Query(c, "limit", 50)
	repo := fiber.Query(c, "repo", "")

	runs, err := s.store.ListRuns(c.Context(), repo, limit)
	if err != nil {
		return s.fail(c, http.StatusInternalServerError, err)
	}
	if runs == nil {
		runs = []store.Run{}
	}
	return c.JSON(fiber.Map{"runs": runs, "count": len(runs)})
}

// listSessions exposes the Claude session IDs the daemon has recorded, so a
// past session can be looked up by the issue it was spent on rather than by
// remembering which run it belonged to.
func (s *Server) listSessions(c fiber.Ctx) error {
	repo := fiber.Query(c, "repo", "")
	issue := fiber.Query(c, "issue", 0)
	limit := fiber.Query(c, "limit", 50)

	sessions, err := s.store.ListSessions(c.Context(), repo, issue, limit)
	if err != nil {
		return s.fail(c, http.StatusInternalServerError, err)
	}
	if sessions == nil {
		sessions = []store.Session{}
	}
	return c.JSON(fiber.Map{"sessions": sessions, "count": len(sessions)})
}

func (s *Server) getRun(c fiber.Ctx) error {
	id := fiber.Params(c, "id", "")
	run, err := s.store.GetRun(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return s.fail(c, http.StatusNotFound, errors.New("no such run"))
	}
	if err != nil {
		return s.fail(c, http.StatusInternalServerError, err)
	}
	events, err := s.store.ListEvents(c.Context(), id)
	if err != nil {
		return s.fail(c, http.StatusInternalServerError, err)
	}
	if events == nil {
		events = []store.Event{}
	}
	return c.JSON(fiber.Map{"run": run, "events": events})
}

// getRunLog serves the raw JSONL transcript. The path comes from the run row,
// never from the request, so this cannot be walked to arbitrary files.
func (s *Server) getRunLog(c fiber.Ctx) error {
	id := fiber.Params(c, "id", "")
	run, err := s.store.GetRun(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return s.fail(c, http.StatusNotFound, errors.New("no such run"))
	}
	if err != nil {
		return s.fail(c, http.StatusInternalServerError, err)
	}
	if run.LogPath == "" {
		return s.fail(c, http.StatusNotFound, errors.New("run has no transcript"))
	}
	if _, err := os.Stat(run.LogPath); err != nil {
		return s.fail(c, http.StatusNotFound, errors.New("transcript file is gone"))
	}
	c.Set("Content-Type", "application/x-ndjson; charset=utf-8")
	return c.SendFile(run.LogPath)
}

func (s *Server) pause(c fiber.Ctx) error {
	var body struct {
		Reason string `json:"reason"`
	}
	_ = c.Bind().Body(&body) // an empty body is fine

	if err := s.gate.Pause(c.Context(), body.Reason); err != nil {
		return s.fail(c, http.StatusInternalServerError, err)
	}
	s.log.Info("paused by operator", "reason", body.Reason)
	s.discord.Paused(body.Reason)
	return c.JSON(fiber.Map{
		"paused": true,
		"note":   "in-flight runs continue; no new issues will be claimed",
	})
}

func (s *Server) resume(c fiber.Ctx) error {
	if err := s.gate.Resume(c.Context()); err != nil {
		return s.fail(c, http.StatusInternalServerError, err)
	}
	s.log.Info("resumed by operator")
	s.discord.Resumed()
	return c.JSON(fiber.Map{"paused": false})
}

func (s *Server) cancelRun(c fiber.Ctx) error {
	id := fiber.Params(c, "id", "")
	if s.ctrl == nil || !s.ctrl.Cancel(id) {
		return s.fail(c, http.StatusNotFound, errors.New("run is not in flight on this process"))
	}
	// The run's own outcome notification follows from the orchestrator, and
	// carries the repo, issue, and attempt that this handler does not know.
	// The run's own outcome notification follows from the orchestrator, and
	// carries the repo, issue, and attempt that this handler does not know.
	s.log.Info("run cancelled by operator", "run", id)
	return c.JSON(fiber.Map{"cancelled": true, "run": id})
}

// getConfig returns the daemon's configuration for the web console, with the
// Discord webhook URL blanked: it is a secret, everything else here is not.
func (s *Server) getConfig(c fiber.Ctx) error {
	cfg := s.cfg
	webhookSet := cfg.Discord.WebhookURL != ""
	cfg.Discord.WebhookURL = ""
	return c.JSON(fiber.Map{
		"config":              cfg,
		"discord_webhook_set": webhookSet,
	})
}

// getModels reports the full model registry plus which models are currently
// cooled down, so the console can show the plan/implement ladders with
// sidelined models struck through rather than silently missing.
func (s *Server) getModels(c fiber.Ctx) error {
	if s.registry == nil {
		return c.JSON(fiber.Map{"models": []models.Model{}, "cooled_down": []string{}, "plan": []models.Model{}, "implement": []models.Model{}})
	}
	cooled, err := s.store.CooledDownModels(c.Context())
	if err != nil {
		s.log.Warn("cooldown lookup failed", "error", err)
		cooled = nil
	}
	cooledList := make([]string, 0, len(cooled))
	for id := range cooled {
		cooledList = append(cooledList, id)
	}
	return c.JSON(fiber.Map{
		"models":      s.registry.Models,
		"cooled_down": cooledList,
		"plan":        s.registry.Ladder(models.RolePlan, cooled),
		"implement":   s.registry.Ladder(models.RoleImplement, cooled),
	})
}

// pollNow asks the orchestrator for a discovery pass right away, rather than
// waiting for the next tick of github.poll_interval.
func (s *Server) pollNow(c fiber.Ctx) error {
	if s.ctrl == nil {
		return s.fail(c, http.StatusServiceUnavailable, errors.New("no controller attached"))
	}
	if !s.ctrl.Poll() {
		return s.fail(c, http.StatusConflict, errors.New("a discovery pass is already queued"))
	}
	return c.JSON(fiber.Map{"queued": true})
}

func (s *Server) fail(c fiber.Ctx, code int, err error) error {
	if code >= 500 {
		s.log.Error("api error", "path", c.Path(), "error", err)
	}
	return c.Status(code).JSON(fiber.Map{"error": err.Error()})
}
