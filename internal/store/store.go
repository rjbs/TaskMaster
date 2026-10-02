// Package store owns the SQLite database: opening it, applying the schema,
// and handing out transactions that track JMAP state (modification
// sequence) counters per data type.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"sync"

	_ "modernc.org/sqlite"
)

// DB wraps the SQLite handle.  All access goes through transactions from
// Begin, which are serialized by a mutex; this is a single-user server and
// SQLite only allows one writer anyway.
type DB struct {
	sql      *sql.DB
	mu       sync.Mutex
	onCommit func(changed map[string]string)
}

// Open opens (creating if needed) the database at path.  The special path
// ":memory:" gives a private in-memory database, used by tests.
func Open(path string) (*DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	if path != ":memory:" {
		dsn += "&_pragma=journal_mode(WAL)"
	}
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	sqldb.SetMaxOpenConns(1)
	if err := migrate(sqldb); err != nil {
		sqldb.Close()
		return nil, err
	}
	return &DB{sql: sqldb}, nil
}

// migrate applies any migrations the database has not yet seen.
func migrate(sqldb *sql.DB) error {
	var version int
	if err := sqldb.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this build understands (%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := sqldb.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("applying schema migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// OnCommit registers a function called after any transaction commits having
// changed state; it receives the new state string for each changed type.
func (d *DB) OnCommit(fn func(changed map[string]string)) {
	d.onCommit = fn
}

func (d *DB) Close() error {
	return d.sql.Close()
}

// Tx is a database transaction that also records which JMAP data types it
// has changed, so the commit hook can announce new states.
type Tx struct {
	*sql.Tx
	db      *DB
	changed map[string]int64
	done    bool
}

// Begin starts a transaction, blocking until any other transaction on this
// DB has finished.
func (d *DB) Begin(ctx context.Context) (*Tx, error) {
	d.mu.Lock()
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		d.mu.Unlock()
		return nil, err
	}
	return &Tx{Tx: tx, db: d, changed: map[string]int64{}}, nil
}

func (t *Tx) finish() {
	if !t.done {
		t.done = true
		t.db.mu.Unlock()
	}
}

func (t *Tx) Commit() error {
	err := t.Tx.Commit()
	t.finish()
	if err == nil && len(t.changed) > 0 && t.db.onCommit != nil {
		states := make(map[string]string, len(t.changed))
		for typ, n := range t.changed {
			states[typ] = StateString(n)
		}
		t.db.onCommit(states)
	}
	return err
}

// Rollback abandons the transaction.  It is safe to call after Commit, so
// callers can defer it.
func (t *Tx) Rollback() error {
	if t.done {
		return nil
	}
	err := t.Tx.Rollback()
	t.finish()
	return err
}

// State returns the current modification sequence for a data type.  A type
// that has never changed is at 0.
func (t *Tx) State(typ string) (int64, error) {
	var n int64
	err := t.QueryRow(`SELECT modseq FROM state WHERE type = ?`, typ).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}

// NextModSeq allocates the next modification sequence number for a data
// type.  Every changed object gets its own number, so a /changes response
// can be truncated at any point without splitting a sequence number.
func (t *Tx) NextModSeq(typ string) (int64, error) {
	var n int64
	err := t.QueryRow(`
		INSERT INTO state (type, modseq) VALUES (?, 1)
		ON CONFLICT (type) DO UPDATE SET modseq = modseq + 1
		RETURNING modseq`, typ).Scan(&n)
	if err != nil {
		return 0, err
	}
	t.changed[typ] = n
	return n, nil
}

// StateString formats a modification sequence as a JMAP state string.
func StateString(n int64) string {
	return strconv.FormatInt(n, 10)
}

// ParseState reverses StateString.
func ParseState(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n >= 0
}
