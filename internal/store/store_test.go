package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// openAtVersion creates a database that has run only the first n
// migrations, as an older build would have left it.
func openAtVersion(t *testing.T, path string, n int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = ` + StateString(int64(n))); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestMigrateFromVersion1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")
	old := openAtVersion(t, path, 1)
	_, err := old.Exec(`INSERT INTO tasks (id, uid, data, start_utc, due_utc, progress, progress_rank,
		created, updated, created_modseq, updated_modseq)
		VALUES ('T-1', 'u1', ?, '2026-10-05T13:00:00Z', NULL, 'needs-action', 0,
		'2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z', 1, 1)`,
		`{"@type":"Task","uid":"u1","start":"2026-10-05T09:00:00","timeZone":"America/New_York"}`)
	if err != nil {
		t.Fatal(err)
	}
	old.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("opening old database: %v", err)
	}
	defer db.Close()

	var version int
	if err := db.sql.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != len(migrations) {
		t.Errorf("user_version = %d, %v; want %d", version, err, len(migrations))
	}
	var start, tz string
	var due sql.NullString
	err = db.sql.QueryRow(`SELECT start_local, due_local, time_zone FROM tasks WHERE id = 'T-1'`).Scan(&start, &due, &tz)
	if err != nil || start != "2026-10-05T09:00:00" || due.Valid || tz != "America/New_York" {
		t.Errorf("backfill: start=%q due=%v tz=%q err=%v", start, due, tz, err)
	}
	if _, err := db.sql.Exec(`SELECT start_utc FROM tasks`); err == nil {
		t.Error("start_utc column should have been dropped")
	}

	// Opening again is a no-op.
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopening migrated database: %v", err)
	}
	db2.Close()
}

func TestRefuseNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.sqlite")
	openAtVersion(t, path, len(migrations)).Close()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`PRAGMA user_version = 99`)
	db.Close()
	if _, err := Open(path); err == nil {
		t.Error("expected an error opening a database from the future")
	}
}

// insertTaskRow adds a task as a version-3 database would hold it, with
// the given JSON data and modseqs; destroyed is 0 for a live task.
func insertTaskRow(t *testing.T, db *sql.DB, id, data string, modseq, destroyed int64) {
	t.Helper()
	var d any
	if destroyed != 0 {
		d = destroyed
	}
	_, err := db.Exec(`INSERT INTO tasks (id, uid, data, progress, progress_rank,
		created, updated, created_modseq, updated_modseq, destroyed_modseq)
		VALUES (?, ?, ?, 'needs-action', 0, '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z', ?, ?, ?)`,
		id, id, data, modseq, modseq, d)
	if err != nil {
		t.Fatal(err)
	}
}

// checkTaskRow asserts a task's stored "version" ("" for none) and
// updated_modseq.
func checkTaskRow(t *testing.T, db *DB, id, wantVersion string, wantModSeq int64) {
	t.Helper()
	var version sql.NullString
	var modseq int64
	err := db.sql.QueryRow(`SELECT json_extract(data, '$.version'), updated_modseq FROM tasks WHERE id = ?`, id).
		Scan(&version, &modseq)
	if err != nil || version.String != wantVersion || modseq != wantModSeq {
		t.Errorf("%s: version=%q modseq=%d err=%v; want version=%q modseq=%d",
			id, version.String, modseq, err, wantVersion, wantModSeq)
	}
}

func TestMigrateBackfillsVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")
	old := openAtVersion(t, path, 3)
	insertTaskRow(t, old, "T-a", `{"@type":"Task","uid":"T-a"}`, 1, 0)
	insertTaskRow(t, old, "T-b", `{"@type":"Task","uid":"T-b","version":"2.0"}`, 2, 0)
	insertTaskRow(t, old, "T-c", `{"@type":"Task","uid":"T-c"}`, 3, 0)
	insertTaskRow(t, old, "T-gone", `{"@type":"Task","uid":"T-gone"}`, 5, 5)
	if _, err := old.Exec(`INSERT INTO state (type, modseq) VALUES ('Task', 5)`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("opening old database: %v", err)
	}
	defer db.Close()

	checkTaskRow(t, db, "T-a", "2.0", 6)
	checkTaskRow(t, db, "T-b", "2.0", 2)
	checkTaskRow(t, db, "T-c", "2.0", 7)
	checkTaskRow(t, db, "T-gone", "", 5)

	tx, err := db.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if state, err := tx.State("Task"); err != nil || state != 7 {
		t.Errorf("Task state = %d, %v; want 7", state, err)
	}
}

func TestMigrateVersionWithNoTasks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.sqlite")
	openAtVersion(t, path, 3).Close()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("opening old database: %v", err)
	}
	db.Close()
}
