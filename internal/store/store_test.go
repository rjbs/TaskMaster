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
