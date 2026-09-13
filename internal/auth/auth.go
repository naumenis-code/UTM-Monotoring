// Package auth gates the mutating parts of the dashboard (adding/editing/
// deleting УТМ, settings) behind a single admin password.
//
// The password is only ever read from an environment variable (see
// config.AdminPassword) — it is never written to the database or the
// repository, so a checkout of this project never carries a live secret.
// Sessions are an in-memory random token in an HttpOnly cookie; they don't
// survive a process restart, which is an acceptable trade-off for a small
// internal tool and avoids a session table.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

const (
	cookieName = "utm_dashboard_session"
	sessionTTL = 12 * time.Hour
)

type Manager struct {
	password string

	mu       sync.Mutex
	sessions map[string]time.Time
}

func New(password string) *Manager {
	return &Manager{password: password, sessions: make(map[string]time.Time)}
}

// Configured reports whether an admin password was set at all. When it
// wasn't, the whole panel is left open (a warning is shown in the UI).
func (m *Manager) Configured() bool {
	return m.password != ""
}

// Check does a constant-time comparison against the configured password.
func (m *Manager) Check(password string) bool {
	if m.password == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(password), []byte(m.password)) == 1
}

func (m *Manager) newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Login creates a new session and sets its cookie on the response.
func (m *Manager) Login(w http.ResponseWriter, r *http.Request) {
	token := m.newToken()

	m.mu.Lock()
	m.sessions[token] = time.Now().Add(sessionTTL)
	m.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
	})
}

// Logout invalidates the current session, if any, and clears its cookie.
func (m *Manager) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		m.mu.Lock()
		delete(m.sessions, c.Value)
		m.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

// Authenticated reports whether the request carries a valid, unexpired
// session. When no password is configured, every request passes.
func (m *Manager) Authenticated(r *http.Request) bool {
	if !m.Configured() {
		return true
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	expiry, ok := m.sessions[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(m.sessions, c.Value)
		return false
	}
	return true
}
