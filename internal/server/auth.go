package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
)

const (
	// sessionTTL is how long an issued token stays valid without use; a hit
	// slides the expiry forward, so an active tab never gets logged out.
	sessionTTL = 12 * time.Hour
	// throttleWindow/throttleMax bound login attempts: once throttleMax
	// failures land inside throttleWindow, further attempts are rejected
	// with 429 until the window rolls over.
	throttleWindow = time.Minute
	throttleMax    = 10
)

// sessionStore tracks issued session tokens and throttles failed logins. A
// zero-value store (Password unset) is never consulted for anything but
// stays inert either way.
type sessionStore struct {
	mu     sync.Mutex
	tokens map[string]time.Time

	failWindowStart time.Time
	failCount       int
}

func newSessionStore() *sessionStore {
	return &sessionStore{tokens: map[string]time.Time{}}
}

// issue mints a new session token.
func (s *sessionStore) issue() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing means the platform's entropy source is broken;
		// there is no sane fallback that keeps the token unguessable.
		panic("server: crypto/rand unavailable: " + err.Error())
	}
	token := base64.RawURLEncoding.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = time.Now().Add(sessionTTL)
	return token
}

// valid reports whether token is a live session, sliding its expiry forward
// on a hit and pruning expired entries as it goes.
func (s *sessionStore) valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for t, exp := range s.tokens {
		if now.After(exp) {
			delete(s.tokens, t)
		}
	}
	exp, ok := s.tokens[token]
	if !ok || now.After(exp) {
		return false
	}
	s.tokens[token] = now.Add(sessionTTL)
	return true
}

// revoke drops token, if present. A caller logging out with an unknown or
// already-expired token is not an error.
func (s *sessionStore) revoke(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, token)
}

// throttled reports whether the failed-login window is currently exhausted.
func (s *sessionStore) throttled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.failWindowStart) > throttleWindow {
		return false
	}
	return s.failCount >= throttleMax
}

// recordFailure counts one failed login attempt toward the throttle window.
func (s *sessionStore) recordFailure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.failWindowStart) > throttleWindow {
		s.failWindowStart = time.Now()
		s.failCount = 0
	}
	s.failCount++
}

// authEnabled reports whether a password has been configured. Whitespace-only
// values count as unset, so "   " can't become an unusable password by accident.
func (s *Server) authEnabled() bool {
	return strings.TrimSpace(s.cfg.Server.Password) != ""
}

// bearerToken extracts the credential from "Authorization: Bearer <value>".
func bearerToken(c fiber.Ctx) string {
	auth := c.Get(fiber.HeaderAuthorization)
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return ""
	}
	return strings.TrimPrefix(auth, prefix)
}

// authenticated reports whether token is either a live session or the
// configured password itself, compared in constant time.
func (s *Server) authenticated(token string) bool {
	if token == "" {
		return false
	}
	if s.sessions.valid(token) {
		return true
	}
	password := strings.TrimSpace(s.cfg.Server.Password)
	return subtle.ConstantTimeCompare([]byte(token), []byte(password)) == 1
}
