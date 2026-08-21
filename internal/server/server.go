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
	"net/http"
	"os"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/ableinc/coding-agent-loop/internal/discord"
	"github.com/ableinc/coding-agent-loop/internal/gate"
	"github.com/ableinc/coding-agent-loop/internal/store"
)

// Controller is the slice of the orchestrator the API needs. Keeping it an
// interface here keeps the packages independent and makes the handlers
// testable without a live loop.
type Controller interface {
	Cancel(runID string) bool
	ActiveRepos() []string
}

// Server wraps the Fiber app.
type Server struct {
	app     *fiber.App
	addr    string
	store   *store.Store
	gate    *gate.Gate
	ctrl    Controller
	log     *slog.Logger
	start   time.Time
	discord *discord.Notifier
}

// New builds the API.
func New(addr string, st *store.Store, g *gate.Gate, ctrl Controller, log *slog.Logger, d *discord.Notifier) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		app:     fiber.New(fiber.Config{AppName: "coding-agent-loop"}),
		addr:    addr,
		store:   st,
		gate:    g,
		ctrl:    ctrl,
		log:     log,
		start:   time.Now(),
		discord: d,
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.app.Get("/healthz", s.health)
	s.app.Get("/status", s.status)
	s.app.Get("/runs", s.listRuns)
	s.app.Get("/runs/:id", s.getRun)
	s.app.Get("/runs/:id/log", s.getRunLog)
	s.app.Post("/pause", s.pause)
	s.app.Post("/resume", s.resume)
	s.app.Post("/runs/:id/cancel", s.cancelRun)
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
	s.log.Info("run cancelled by operator", "run", id)
	return c.JSON(fiber.Map{"cancelled": true, "run": id})
}

func (s *Server) fail(c fiber.Ctx, code int, err error) error {
	if code >= 500 {
		s.log.Error("api error", "path", c.Path(), "error", err)
	}
	return c.Status(code).JSON(fiber.Map{"error": err.Error()})
}
