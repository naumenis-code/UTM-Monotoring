// Package db opens the SQLite database and applies the schema.
package db

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

// Open creates the parent directory for path if needed, opens the SQLite
// database and applies the schema (idempotent).
func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
	}

	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// modernc.org/sqlite does not support concurrent writers well; a single
	// connection avoids "database is locked" errors for this low-traffic app.
	sqlDB.SetMaxOpenConns(1)

	if _, err := sqlDB.Exec(schema); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	if err := migrate(sqlDB); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}

	return sqlDB, nil
}

// migrate adds columns introduced after the initial schema to databases
// created by an older version of the app. CREATE TABLE IF NOT EXISTS above
// only handles brand-new databases, so upgrades need explicit ALTER TABLE
// statements; each is a no-op (its "duplicate column" error is swallowed)
// when the column already exists.
func migrate(sqlDB *sql.DB) error {
	stmts := []string{
		`ALTER TABLE settings ADD COLUMN telegram_mode TEXT NOT NULL DEFAULT 'direct'`,
		`ALTER TABLE settings ADD COLUMN telegram_proxy_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE settings ADD COLUMN telegram_relay_base_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE settings ADD COLUMN telegram_relay_auth_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE telegram_chats ADD COLUMN utm_id INTEGER REFERENCES utms(id) ON DELETE CASCADE`,
		`ALTER TABLE utms ADD COLUMN fsrar_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE utms ADD COLUMN consecutive_poll_failures INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE utms ADD COLUMN failing_since TEXT`,
	}
	for _, stmt := range stmts {
		if _, err := sqlDB.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return nil
}
