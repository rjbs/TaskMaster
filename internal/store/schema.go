package store

// Destroyed objects are kept as tombstones (destroyed_modseq set) so that
// /changes can report them; every query for live objects must filter on
// destroyed_modseq IS NULL.
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
}
