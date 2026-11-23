// Package store owns the SQLite database: opening it, applying the schema,
// and handing out transactions.  JMAP state, which is to say each type's
// modseq counter and the tombstones of what has been destroyed, is kept by
// Gizmo's sqlitekit, in tables of its own.
package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/rjbs/gizmo/sqlitekit"
)

// The data types that have JMAP state.
const (
	TypeTaskList = "TaskList"
	TypeTask     = "Task"
)

// Kit is Gizmo's state kit as TaskMaster uses it.
var Kit = sqlitekit.New(
	sqlitekit.Type{Name: TypeTaskList, Table: "tasklists"},
	sqlitekit.Type{Name: TypeTask, Table: "tasks"},
)

// DB wraps the SQLite handle.  All access goes through transactions from
// Begin and Read.
type DB struct {
	sql      *sql.DB
	onCommit func(changed map[string]string)
}

// Open opens (creating if needed) the database at path.  The special path
// ":memory:" gives a private in-memory database, used by tests.
func Open(path string) (*DB, error) {
	var sqldb *sql.DB
	var err error
	if path == ":memory:" {
		// An in-memory database belongs to the one connection that
		// made it, so there can only be the one.
		sqldb, err = sql.Open("sqlite", "file::memory:?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate")
		if err == nil {
			sqldb.SetMaxOpenConns(1)
		}
	} else {
		sqldb, err = sqlitekit.Open(path)
	}
	if err != nil {
		return nil, err
	}
	if err := prepare(context.Background(), sqldb); err != nil {
		sqldb.Close()
		return nil, err
	}
	return &DB{sql: sqldb}, nil
}

// prepare brings a database up to date: its own schema, then Gizmo's state
// tables, then whatever of its old state is waiting to be moved into them.
func prepare(ctx context.Context, sqldb *sql.DB) error {
	if err := sqlitekit.Migrate(ctx, sqldb, migrations); err != nil {
		return err
	}
	if err := Kit.Init(ctx, sqldb); err != nil {
		return err
	}
	return adoptKit(ctx, sqldb)
}

// adoptKit finishes moving a database that kept its own state onto Gizmo's
// state tables, which Kit.Init has just made.  The migration that took the
// tombstones out of the tasks and tasklists tables left them in
// old_tombstone, and the old counters are still in the state table.  Both
// are copied over here and then dropped, so this runs once.  Each type's
// counter goes on from where it was.
//
// State strings do change: they gain the database's epoch, which is new.
// Every client fetches everything again, once.  -- claude, 2026-11-22
func adoptKit(ctx context.Context, sqldb *sql.DB) error {
	var n int
	if err := sqldb.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name = 'old_tombstone'`).Scan(&n); err != nil || n == 0 {
		return err
	}
	tx, err := sqldb.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, step := range []string{
		`INSERT INTO gizmo_tombstone (type, id, created_modseq, modseq, destroyed_at)
		 SELECT type, id, created_modseq, modseq, unixepoch() FROM old_tombstone`,
		`UPDATE gizmo_state SET highest_modseq = max(highest_modseq,
		   coalesce((SELECT modseq FROM state WHERE state.type = gizmo_state.type), 0))`,
		`DROP TABLE old_tombstone`,
		`DROP TABLE state`,
	} {
		if _, err := tx.ExecContext(ctx, step); err != nil {
			return fmt.Errorf("moving state to Gizmo's tables: %w", err)
		}
	}
	return tx.Commit()
}

// OnCommit registers a function called after any transaction commits having
// changed state; it receives the new state string for each changed type.
func (d *DB) OnCommit(fn func(changed map[string]string)) {
	d.onCommit = fn
}

func (d *DB) Close() error {
	return d.sql.Close()
}

// Tx is a database transaction: Gizmo's, which is a *sql.Tx that knows each
// type's state, with the kit's operations in the form this server's code
// calls them, which has no context to pass.
type Tx struct {
	*sqlitekit.Tx
}

// Begin starts a transaction that may write.
func (d *DB) Begin(ctx context.Context) (*Tx, error) {
	tx, err := Kit.Begin(ctx, d.sql, d.onCommit)
	if err != nil {
		return nil, err
	}
	return &Tx{tx}, nil
}

// Read starts a transaction that only reads.
func (d *DB) Read(ctx context.Context) (*Tx, error) {
	tx, err := Kit.Read(ctx, d.sql)
	if err != nil {
		return nil, err
	}
	return &Tx{tx}, nil
}

// States returns the state string of every type.
func (d *DB) States(ctx context.Context) (map[string]string, error) {
	return Kit.States(ctx, d.sql)
}

// NextModSeq allocates the next modification sequence number for a data
// type.  Every changed object gets its own number, so a /changes response
// can be truncated at any point without splitting a sequence number.
func (t *Tx) NextModSeq(typ string) (int64, error) {
	return t.Tx.NextModseq(context.Background(), typ)
}

// Creating prepares for a new object, and returns the modseq to write as
// both its created_modseq and its modseq.
func (t *Tx) Creating(typ, id string) (int64, error) {
	return t.Tx.Create(context.Background(), typ, id)
}

// Touch gives an object a new modseq, to mark it changed.
func (t *Tx) Touch(typ, id string) error {
	_, err := t.Tx.Touch(context.Background(), typ, id)
	return err
}

// Destroy deletes an object's row and leaves a tombstone for /changes.
// Rows that refer to the object must be dealt with first.
func (t *Tx) Destroy(typ, id string) error {
	_, err := t.Tx.Destroy(context.Background(), typ, id)
	return err
}
