package store

// The tasks and tasklists tables hold only live objects.  What has been
// destroyed leaves a tombstone in Gizmo's tables, for /changes to report.
// (The first migration here kept destroyed objects in place, marked with a
// destroyed_modseq; the last one takes them out.)
//
// migrations is applied in order; PRAGMA user_version records how many have
// run.  Never edit an entry that has shipped; append a new one.
var migrations = []string{
	`
CREATE TABLE IF NOT EXISTS state (
	type   TEXT PRIMARY KEY,
	modseq INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS tasklists (
	id                          TEXT PRIMARY KEY,
	name                        TEXT NOT NULL,
	description                 TEXT,
	color                       TEXT,
	sort_order                  INTEGER NOT NULL DEFAULT 0,
	is_subscribed               INTEGER NOT NULL DEFAULT 1,
	is_visible                  INTEGER NOT NULL DEFAULT 1,
	is_default                  INTEGER NOT NULL DEFAULT 0,
	default_alerts_with_time    TEXT,
	default_alerts_without_time TEXT,
	time_zone                   TEXT,
	created_modseq              INTEGER NOT NULL,
	updated_modseq              INTEGER NOT NULL,
	destroyed_modseq            INTEGER
);

CREATE INDEX IF NOT EXISTS tasklists_updated ON tasklists (updated_modseq);

CREATE TABLE IF NOT EXISTS tasks (
	id               TEXT PRIMARY KEY,
	uid              TEXT NOT NULL,
	recurrence_id    TEXT,
	data             TEXT NOT NULL,
	start_utc        TEXT,
	due_utc          TEXT,
	progress         TEXT NOT NULL,
	progress_rank    INTEGER NOT NULL,
	percent_complete INTEGER,
	priority         INTEGER,
	created          TEXT NOT NULL,
	updated          TEXT NOT NULL,
	created_modseq   INTEGER NOT NULL,
	updated_modseq   INTEGER NOT NULL,
	destroyed_modseq INTEGER
);

CREATE UNIQUE INDEX IF NOT EXISTS tasks_uid_recurrence
	ON tasks (uid, COALESCE(recurrence_id, ''))
	WHERE destroyed_modseq IS NULL;

CREATE INDEX IF NOT EXISTS tasks_updated ON tasks (updated_modseq);

CREATE TABLE IF NOT EXISTS task_lists (
	task_id TEXT NOT NULL REFERENCES tasks (id),
	list_id TEXT NOT NULL REFERENCES tasklists (id),
	PRIMARY KEY (task_id, list_id)
);

CREATE INDEX IF NOT EXISTS task_lists_list ON task_lists (list_id);

CREATE VIRTUAL TABLE IF NOT EXISTS tasks_fts USING fts5 (
	task_id UNINDEXED,
	title,
	description,
	other,
	tokenize = 'porter unicode61'
);
`,
	// The draft now resolves zoneless tasks in the Task/query caller's
	// time zone, so instants can no longer be computed when a task is
	// written.  Keep the local date-times and the task's own zone instead.
	// -- claude, 2026-10-02
	`
ALTER TABLE tasks ADD COLUMN start_local TEXT;
ALTER TABLE tasks ADD COLUMN due_local TEXT;
ALTER TABLE tasks ADD COLUMN time_zone TEXT;
UPDATE tasks SET
	start_local = json_extract(data, '$.start'),
	due_local   = json_extract(data, '$.due'),
	time_zone   = json_extract(data, '$.timeZone');
ALTER TABLE tasks DROP COLUMN start_utc;
ALTER TABLE tasks DROP COLUMN due_utc;
`,
	// Vendor-prefixed TaskList properties, stored as one JSON object, or
	// NULL when there are none.  -- claude, 2026-11-06
	`
ALTER TABLE tasklists ADD COLUMN vendor_props TEXT;
`,
	// JSCalendar bis reads a Task with no "version" as 1.0, which every
	// Task this server stored before it began setting one is not.  Each
	// changed Task gets its own modseq, as NextModSeq would give it, so
	// Task/changes reports it to clients holding the old version.
	// -- claude, 2026-11-10
	`
UPDATE tasks SET
	data = json_set(data, '$.version', '2.0'),
	updated_modseq = seq.n
FROM (
	SELECT id,
		COALESCE((SELECT modseq FROM state WHERE type = 'Task'), 0)
			+ ROW_NUMBER() OVER (ORDER BY id) AS n
	FROM tasks
	WHERE destroyed_modseq IS NULL AND json_type(data, '$.version') IS NULL
) AS seq
WHERE tasks.id = seq.id;

INSERT INTO state (type, modseq)
	SELECT 'Task', MAX(updated_modseq) FROM tasks
	HAVING MAX(updated_modseq) IS NOT NULL
	ON CONFLICT (type) DO UPDATE SET modseq = excluded.modseq
	WHERE excluded.modseq > state.modseq;
`,
	// Versions of indexes whose contents are computed in Go, so that the
	// code can tell when they were built by older rules and rebuild them.
	// -- claude, 2026-11-10
	`
CREATE TABLE index_versions (
	name    TEXT PRIMARY KEY,
	version INTEGER NOT NULL
);
`,
	// JMAP state moves to Gizmo's sqlitekit, which wants a table of live
	// objects with created_modseq and modseq columns, and keeps tombstones
	// itself.  Destroyed tasks and lists, which until now stayed in their
	// tables for good, are deleted here, data and all.  What /changes needs
	// of them is set aside in old_tombstone, from which adoptKit moves it
	// to Gizmo's table once that exists, along with the counters in the
	// state table.  -- claude, 2026-11-22
	`
CREATE TABLE old_tombstone (
	type           TEXT NOT NULL,
	id             TEXT NOT NULL,
	created_modseq INTEGER NOT NULL,
	modseq         INTEGER NOT NULL
);
INSERT INTO old_tombstone
	SELECT 'Task', id, created_modseq, updated_modseq FROM tasks WHERE destroyed_modseq IS NOT NULL;
INSERT INTO old_tombstone
	SELECT 'TaskList', id, created_modseq, updated_modseq FROM tasklists WHERE destroyed_modseq IS NOT NULL;

DELETE FROM task_lists
	WHERE task_id IN (SELECT id FROM old_tombstone WHERE type = 'Task')
	   OR list_id IN (SELECT id FROM old_tombstone WHERE type = 'TaskList');
DELETE FROM tasks_fts WHERE task_id IN (SELECT id FROM old_tombstone WHERE type = 'Task');
DELETE FROM tasks WHERE destroyed_modseq IS NOT NULL;
DELETE FROM tasklists WHERE destroyed_modseq IS NOT NULL;

DROP INDEX tasks_uid_recurrence;
ALTER TABLE tasks DROP COLUMN destroyed_modseq;
ALTER TABLE tasklists DROP COLUMN destroyed_modseq;
CREATE UNIQUE INDEX tasks_uid_recurrence ON tasks (uid, COALESCE(recurrence_id, ''));

ALTER TABLE tasks RENAME COLUMN updated_modseq TO modseq;
ALTER TABLE tasklists RENAME COLUMN updated_modseq TO modseq;
`,
}
