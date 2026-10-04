// Package database prepares the SQLite file and opens it under tracing.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/XSAM/otelsql"
	// The pure-Go driver: no cgo, so the binary stays static.
	_ "modernc.org/sqlite"
)

// The example has no migrations: the table is created on startup.
const schema = `
CREATE TABLE IF NOT EXISTS posts (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL,
    title      TEXT NOT NULL,
    body       TEXT NOT NULL,
    saved_at   TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
)`

// Prepare switches the file to WAL and creates the table, once, before serving.
func Prepare(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	for _, statement := range []string{"PRAGMA journal_mode=WAL", schema} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return errors.Join(fmt.Errorf("prepare %s: %w", path, err), db.Close())
		}
	}
	return db.Close()
}

// Open opens the file with a span per statement, named after its verb — INSERT, SELECT.
func Open(path string) (*sql.DB, error) {
	db, err := otelsql.Open("sqlite", path,
		otelsql.WithSpanNameFormatter(statementVerb),
		// One span per statement: none for preparing, resetting a session or reading rows.
		otelsql.WithSpanOptions(otelsql.SpanOptions{OmitConnPrepare: true, OmitConnResetSession: true, OmitRows: true}),
	)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// SQLite writes from one connection at a time: one connection keeps `database is locked` away.
	db.SetMaxOpenConns(1)
	return db, nil
}

func statementVerb(_ context.Context, method otelsql.Method, query string) string {
	if verb, _, ok := strings.Cut(strings.TrimSpace(query), " "); ok {
		return strings.ToUpper(verb)
	}
	return string(method)
}
