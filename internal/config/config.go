// Package config reads process-level configuration from the environment.
//
// Everything that an operator might want to change without restarting the
// process (Telegram token, polling schedule, chat list) lives in the
// database instead and is edited from the "Настройки" web page.
package config

import (
	"os"
	"time"
)

type Config struct {
	// HTTPAddr is the address the dashboard's own web server listens on,
	// e.g. ":8088". Deliberately not 8080, which is the УТМ's own default
	// port, to avoid confusion when both run on the same host.
	HTTPAddr string
	// DBPath is the path to the SQLite database file.
	DBPath string
	// PollHTTPTimeout bounds a single request to a УТМ's HTTP API.
	PollHTTPTimeout time.Duration
	// AdminPassword, when set, gates adding/editing/deleting УТМ and the
	// settings page behind a login form. Deliberately read only from the
	// environment (never stored in the repo or the database) so it never
	// ends up committed to source control. Empty means the panel is left
	// open — the UI shows a persistent warning in that case.
	AdminPassword string
}

func Load() Config {
	return Config{
		HTTPAddr:        getEnv("HTTP_ADDR", ":8088"),
		DBPath:          getEnv("DB_PATH", "./data/utm-dashboard.db"),
		PollHTTPTimeout: 10 * time.Second,
		AdminPassword:   os.Getenv("ADMIN_PASSWORD"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
