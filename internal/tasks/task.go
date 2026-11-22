package tasks

import (
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/rjbs/gizmo/jmap"
	"github.com/rjbs/taskmaster/internal/store"
)

// task is a stored Task.  Data is the JSCalendar object without the JMAP
// additions (id, taskListIds), which are held in the other fields.
type task struct {
	ID           string
	UID          string
	RecurrenceID *string
	Data         map[string]any
	Lists        []string
}

func (t *task) toJMAP() map[string]any {
	m := maps.Clone(t.Data)
	m["id"] = t.ID
	lists := make(map[string]any, len(t.Lists))
	for _, l := range t.Lists {
		lists[l] = true
	}
	m["taskListIds"] = lists
	return m
}

// progressRank orders progress values for sorting.  The spec does not
// define this order; we go from least to most finished, with unknown
// values last.  -- claude, 2026-10-02
var progressRank = map[string]int64{
	"needs-action": 0,
	"in-process":   1,
	"completed":    2,
	"failed":       3,
	"cancelled":    4,
}

// effectiveProgress returns the "progress" property, or the default
// computed from participants per JSCalendar bis §4.2.5 when it is absent.
func effectiveProgress(obj map[string]any) string {
	if p, ok := obj["progress"].(string); ok && p != "" {
		return p
	}
	participants, _ := obj["participants"].(map[string]any)
	if len(participants) == 0 {
		return "needs-action"
	}
	allCompleted, anyFailed, anyInProcess := true, false, false
	for _, v := range participants {
		p, _ := v.(map[string]any)
		switch p["progress"] {
		case "completed":
		case "failed":
			allCompleted, anyFailed = false, true
		case "in-process":
			allCompleted, anyInProcess = false, true
		default:
			allCompleted = false
		}
	}
	switch {
	case allCompleted:
		return "completed"
	case anyFailed:
		return "failed"
	case anyInProcess:
		return "in-process"
	}
	return "needs-action"
}

// jscalendarVersion is the JSCalendar version every stored Task conforms
// to, set by the server when a create omits it.
const jscalendarVersion = "2.0"

// validateTask checks the typed properties of a JSCalendar Task object.
// Properties this server does not interpret are stored as given, including
// ones it has never heard of: JSCalendar allows vendor extensions, and
// rejecting unknown names would make the server a worse archive than a
// client expects.  -- claude, 2026-10-02
func validateTask(obj map[string]any) *jmap.SetError {
	var bad, why []string
	fail := func(prop, msg string) {
		bad = append(bad, prop)
		why = append(why, prop+": "+msg)
	}
	has := func(prop string) bool {
		_, ok := obj[prop]
		return ok
	}

	if t, ok := obj["@type"]; ok && t != "Task" {
		fail("@type", "must be \"Task\"")
	}
	// "1.0" is registered too, but means RFC 8984 semantics, which this
	// server neither validates nor produces, and a later version may hold
	// data we cannot check.  Storing either under our rules would misstate
	// what the task contains.
	if obj["version"] != jscalendarVersion {
		fail("version", "must be \""+jscalendarVersion+"\"")
	}
	if uid, ok := obj["uid"].(string); !ok || uid == "" || len(uid) > 255 {
		fail("uid", "must be a non-empty string of at most 255 octets")
	}
	if has("method") {
		fail("method", "must not be set on a stored Task")
	}

	for _, prop := range []string{
		"title", "description", "descriptionContentType", "color", "locale",
		"prodId", "mainLocationId", "freeBusyStatus",
	} {
		if v, ok := obj[prop]; ok {
			if _, ok := v.(string); !ok {
				fail(prop, "must be a string")
			}
		}
	}
	if v, ok := obj["privacy"]; ok {
		if p, _ := v.(string); p != "public" && p != "private" && p != "secret" {
			fail("privacy", "must be public, private or secret")
		}
	}
	if v, ok := obj["progress"]; ok {
		if p, _ := v.(string); p == "" {
			fail("progress", "must be a non-empty string")
		}
	}
	for _, prop := range []string{"start", "due", "recurrenceId"} {
		if v, ok := obj[prop]; ok {
			if s, _ := v.(string); s == "" {
				fail(prop, "must be a LocalDateTime")
			} else if _, ok := parseLocalDateTime(s); !ok {
				fail(prop, "must be a LocalDateTime")
			}
		}
	}
	for _, prop := range []string{"created", "updated", "progressUpdated"} {
		if v, ok := obj[prop]; ok {
			s, _ := v.(string)
			if _, ok := parseUTCDateTime(s); !ok {
				fail(prop, "must be a UTCDateTime")
			}
		}
	}
	for _, prop := range []string{"timeZone", "recurrenceIdTimeZone"} {
		if v, ok := obj[prop]; ok {
			s, _ := v.(string)
			if _, ok := loadZone(s); !ok {
				fail(prop, "must be an IANA time zone id")
			}
		}
	}
	for _, prop := range []string{"showWithoutTime", "useDefaultAlerts"} {
		if v, ok := obj[prop]; ok {
			if _, ok := v.(bool); !ok {
				fail(prop, "must be a boolean")
			}
		}
	}
	if v, ok := obj["estimatedDuration"]; ok {
		if s, _ := v.(string); !validDuration(s, false) {
			fail("estimatedDuration", "must be a Duration")
		}
	}
	intProp := func(prop string, lo, hi int64) {
		if v, ok := obj[prop]; ok {
			if n, ok := intValue(v); !ok || n < lo || n > hi {
				fail(prop, fmt.Sprintf("must be an integer from %d to %d", lo, hi))
			}
		}
	}
	intProp("percentComplete", 0, 100)
	intProp("priority", 0, 9)
	intProp("sequence", 0, 1<<53)

	for _, prop := range []string{
		"relatedTo", "locations", "virtualLocations", "links", "keywords",
		"categories", "recurrenceRule", "recurrenceOverrides",
		"participants", "alerts",
	} {
		if v, ok := obj[prop]; ok {
			if _, ok := v.(map[string]any); !ok {
				fail(prop, "must be an object")
			}
		}
	}
	for _, prop := range []string{"keywords", "categories"} {
		if m, ok := obj[prop].(map[string]any); ok {
			for k, v := range m {
				if v != true {
					fail(prop, "the value for "+k+" must be true")
					break
				}
			}
		}
	}

	if (has("recurrenceRule") || has("recurrenceId")) && !has("start") {
		fail("start", "is required when recurrenceRule or recurrenceId is set")
	}
	// JSCalendar bis §4.2 states this rule with the timeZone condition
	// inverted, contradicting its own changelog.  We follow the evident
	// intent: a time zone or all-day flag needs a date to apply to.
	// -- claude, 2026-10-02
	showWithoutTime, _ := obj["showWithoutTime"].(bool)
	if (has("timeZone") || showWithoutTime) && !has("start") && !has("due") {
		fail("due", "at least one of start and due is required when timeZone or showWithoutTime is set")
	}
	// The instances inherit the base task's alerts, so a base task that
	// fails here would mostly fail again through its overrides.
	if id := unanchoredAlert(obj); id != "" {
		fail("alerts", "alert "+id+" is relative to a time the task does not have")
	} else if rid := unanchoredOverride(obj); rid != "" {
		fail("recurrenceOverrides", "the instance at "+rid+" has an alert relative to a time it does not have")
	}
	// As with alerts, instances inherit the base task's properties.
	if paths := reservedPaths(obj); len(paths) > 0 {
		for _, path := range paths {
			fail(path, "is reserved by JSCalendar")
		}
	} else if rid := reservedOverride(obj); rid != "" {
		fail("recurrenceOverrides", "the instance at "+rid+" sets a property reserved by JSCalendar")
	}

	if len(bad) > 0 {
		return jmap.InvalidProperties(strings.Join(why, "; "), bad...)
	}
	return nil
}

// derivedColumns are the denormalized values kept beside the JSON so that
// Task/query can filter and sort without decoding every task.  Date-times
// stay local, with the task's own zone beside them, because the zone that
// resolves a zoneless task is chosen by the querying client.
type derivedColumns struct {
	startLocal, dueLocal, timeZone *string
	progress                       string
	progressRank                   int64
	percentComplete                *int64
	priority                       *int64
	created, updated               string
}

func derive(obj map[string]any) derivedColumns {
	optString := func(prop string) *string {
		if s, ok := obj[prop].(string); ok {
			return &s
		}
		return nil
	}
	optInt := func(prop string) *int64 {
		if n, ok := intValue(obj[prop]); ok {
			return &n
		}
		return nil
	}

	d := derivedColumns{
		startLocal:      optString("start"),
		dueLocal:        optString("due"),
		timeZone:        optString("timeZone"),
		progress:        effectiveProgress(obj),
		percentComplete: optInt("percentComplete"),
		priority:        optInt("priority"),
		created:         stringOr(obj["created"], ""),
		updated:         stringOr(obj["updated"], ""),
	}
	rank, ok := progressRank[d.progress]
	if !ok {
		rank = int64(len(progressRank))
	}
	d.progressRank = rank
	return d
}

func stringOr(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func loadTask(tx *store.Tx, id string) (*task, error) {
	var data string
	var recurrenceID sql.NullString
	t := &task{ID: id}
	err := tx.QueryRow(`SELECT uid, recurrence_id, data FROM tasks WHERE id = ? AND destroyed_modseq IS NULL`, id).
		Scan(&t.UID, &recurrenceID, &data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if recurrenceID.Valid {
		t.RecurrenceID = &recurrenceID.String
	}
	if t.Data, err = jmap.DecodeObject([]byte(data)); err != nil {
		return nil, fmt.Errorf("task %s has corrupt data: %w", id, err)
	}
	rows, err := tx.Query(`SELECT list_id FROM task_lists WHERE task_id = ? ORDER BY list_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		t.Lists = append(t.Lists, l)
	}
	return t, rows.Err()
}

func allTaskIDs(tx *store.Tx) ([]string, error) {
	rows, err := tx.Query(`SELECT id FROM tasks WHERE destroyed_modseq IS NULL ORDER BY created, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// writeTask inserts or updates the task row, its list memberships, and its
// full-text index entry.
func (s *Service) writeTask(tx *store.Tx, t *task, isNew bool) error {
	d := derive(t.Data)
	data, err := encodeJSON(t.Data)
	if err != nil {
		return err
	}
	modseq, err := tx.NextModSeq(typeTask)
	if err != nil {
		return err
	}
	if isNew {
		_, err = tx.Exec(`INSERT INTO tasks (id, uid, recurrence_id, data, start_local, due_local,
			time_zone, progress, progress_rank, percent_complete, priority, created, updated,
			created_modseq, updated_modseq) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			t.ID, t.UID, t.RecurrenceID, data, d.startLocal, d.dueLocal, d.timeZone, d.progress,
			d.progressRank, d.percentComplete, d.priority, d.created, d.updated, modseq, modseq)
	} else {
		_, err = tx.Exec(`UPDATE tasks SET uid = ?, recurrence_id = ?, data = ?, start_local = ?,
			due_local = ?, time_zone = ?, progress = ?, progress_rank = ?, percent_complete = ?,
			priority = ?, created = ?, updated = ?, updated_modseq = ? WHERE id = ?`,
			t.UID, t.RecurrenceID, data, d.startLocal, d.dueLocal, d.timeZone, d.progress,
			d.progressRank, d.percentComplete, d.priority, d.created, d.updated, modseq, t.ID)
	}
	if err != nil {
		return err
	}

	if _, err := tx.Exec(`DELETE FROM task_lists WHERE task_id = ?`, t.ID); err != nil {
		return err
	}
	for _, l := range t.Lists {
		if _, err := tx.Exec(`INSERT INTO task_lists (task_id, list_id) VALUES (?, ?)`, t.ID, l); err != nil {
			return err
		}
	}

	return writeSearchEntry(tx, t.ID, t.Data)
}

func destroyTask(tx *store.Tx, id string) error {
	modseq, err := tx.NextModSeq(typeTask)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE tasks SET destroyed_modseq = ?, updated_modseq = ? WHERE id = ?`, modseq, modseq, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM task_lists WHERE task_id = ?`, id); err != nil {
		return err
	}
	_, err = tx.Exec(`DELETE FROM tasks_fts WHERE task_id = ?`, id)
	return err
}

// touchTask marks a task as changed without altering its data, used when
// its taskListIds change because a list was destroyed.
func touchTask(tx *store.Tx, id string) error {
	modseq, err := tx.NextModSeq(typeTask)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE tasks SET updated_modseq = ? WHERE id = ?`, modseq, id)
	return err
}

// existingTaskWithUID returns the id of the live task, other than exceptID,
// that has the given uid and recurrenceId, or "" if there is none.  The
// spec forbids two such tasks within an account.
func existingTaskWithUID(tx *store.Tx, uid string, recurrenceID *string, exceptID string) (string, error) {
	rid := ""
	if recurrenceID != nil {
		rid = *recurrenceID
	}
	var id string
	err := tx.QueryRow(`SELECT id FROM tasks WHERE uid = ? AND COALESCE(recurrence_id, '') = ?
		AND destroyed_modseq IS NULL AND id != ?`, uid, rid, exceptID).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

// extractTaskListIDs validates and removes "taskListIds" from obj, resolving
// creation references and checking that every list exists.
func extractTaskListIDs(c *jmap.Call, tx *store.Tx, obj map[string]any) ([]string, *jmap.SetError, error) {
	bad := func(msg string) ([]string, *jmap.SetError, error) {
		return nil, jmap.InvalidProperties("taskListIds: "+msg, "taskListIds"), nil
	}
	raw, ok := obj["taskListIds"].(map[string]any)
	if !ok || len(raw) == 0 {
		return bad("a task must belong to at least one task list")
	}
	delete(obj, "taskListIds")

	var lists []string
	for key, v := range raw {
		if v != true {
			return bad("the value for " + key + " must be true")
		}
		id, ok := c.ResolveID(key)
		if !ok {
			return bad("unresolvable creation reference " + key)
		}
		l, err := loadTaskList(tx, id)
		if err != nil {
			return nil, nil, err
		}
		if l == nil {
			return bad("no such task list " + key)
		}
		lists = append(lists, id)
	}
	slices.Sort(lists)
	return slices.Compact(lists), nil, nil
}

func (s *Service) taskGet(c *jmap.Call, args map[string]any) (map[string]any, error) {
	ids, hasIDs, err := argStringList(args, "ids")
	if err != nil {
		return nil, err
	}
	props, hasProps, err := argStringList(args, "properties")
	if err != nil {
		return nil, err
	}
	var window [2]*string
	for i, key := range []string{"recurrenceOverridesAfter", "recurrenceOverridesBefore"} {
		v, ok, err := argString(args, key)
		if err != nil {
			return nil, err
		}
		if ok {
			if _, valid := parseUTCDateTime(v); !valid {
				return nil, jmap.ArgError("%s must be a UTCDateTime", key)
			}
			window[i] = &v
		}
	}

	tx, derr := s.begin(c)
	if derr != nil {
		return nil, derr
	}
	defer tx.Rollback()

	state, derr := tx.State(typeTask)
	if derr != nil {
		return nil, derr
	}
	if !hasIDs {
		if ids, derr = allTaskIDs(tx); derr != nil {
			return nil, derr
		}
	}

	list := make([]any, 0, len(ids))
	notFound := []string{}
	for _, id := range ids {
		real, ok := c.ResolveID(id)
		if !ok {
			notFound = append(notFound, id)
			continue
		}
		t, derr := loadTask(tx, real)
		if derr != nil {
			return nil, derr
		}
		if t == nil {
			notFound = append(notFound, id)
			continue
		}
		obj := t.toJMAP()
		s.windowOverrides(obj, window[0], window[1])
		if _, ok := obj["useDefaultAlerts"]; !ok {
			obj["useDefaultAlerts"] = false
		}
		if !hasProps {
			delete(obj, "iCalendar")
		}
		list = append(list, filterProperties(obj, props, hasProps))
	}

	return map[string]any{
		"accountId": s.cfg.AccountID,
		"state":     store.StateString(state),
		"list":      list,
		"notFound":  notFound,
	}, nil
}

// windowOverrides drops recurrenceOverrides outside [after, before), both
// UTCDateTime strings, comparing each override's recurrence id as an
// instant in the task's zone.
func (s *Service) windowOverrides(obj map[string]any, after, before *string) {
	overrides, ok := obj["recurrenceOverrides"].(map[string]any)
	if !ok || (after == nil && before == nil) {
		return
	}
	loc := s.cfg.DefaultTimeZone
	if tz, ok := obj["timeZone"].(string); ok {
		if l, ok := loadZone(tz); ok {
			loc = l
		}
	}
	kept := map[string]any{}
	for rid, patch := range overrides {
		local, ok := parseLocalDateTime(rid)
		if !ok {
			continue
		}
		utc := formatUTC(inZone(local, loc))
		if (after == nil || utc >= *after) && (before == nil || utc < *before) {
			kept[rid] = patch
		}
	}
	obj["recurrenceOverrides"] = kept
}

func (s *Service) taskChanges(c *jmap.Call, args map[string]any) (map[string]any, error) {
	tx, err := s.begin(c)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return s.changes(tx, "tasks", typeTask, args)
}

func (s *Service) taskSet(c *jmap.Call, args map[string]any) (map[string]any, error) {
	create, _, err := argObject(args, "create")
	if err != nil {
		return nil, err
	}
	update, _, err := argObject(args, "update")
	if err != nil {
		return nil, err
	}
	destroy, _, err := argStringList(args, "destroy")
	if err != nil {
		return nil, err
	}
	ifInState, hasIfInState, err := argString(args, "ifInState")
	if err != nil {
		return nil, err
	}

	tx, derr := s.begin(c)
	if derr != nil {
		return nil, derr
	}
	defer tx.Rollback()

	oldState, derr := tx.State(typeTask)
	if derr != nil {
		return nil, derr
	}
	if hasIfInState && ifInState != store.StateString(oldState) {
		return nil, &jmap.MethodError{Type: jmap.ErrStateMismatch}
	}

	created := map[string]any{}
	notCreated := map[string]any{}
	updated := map[string]any{}
	notUpdated := map[string]any{}
	destroyed := []string{}
	notDestroyed := map[string]any{}

	// Remembered so that a privacy conflict found after everything has
	// been applied can be unwound for just the tasks involved.
	createdTasks := map[string]*task{}
	previous := map[string]*task{}

	for _, cid := range slices.Sorted(maps.Keys(create)) {
		props, ok := create[cid].(map[string]any)
		if !ok {
			notCreated[cid] = jmap.InvalidProperties("the value to create must be an object")
			continue
		}
		t, serr, derr := s.createTask(c, tx, props)
		if derr != nil {
			return nil, derr
		}
		if serr != nil {
			notCreated[cid] = serr
			continue
		}
		c.SetCreatedID(cid, t.ID)
		createdTasks[cid] = t
		created[cid] = map[string]any{
			"id":      t.ID,
			"@type":   "Task",
			"version": t.Data["version"],
			"uid":     t.UID,
			"created": t.Data["created"],
			"updated": t.Data["updated"],
		}
	}

	for _, id := range slices.Sorted(maps.Keys(update)) {
		patch, ok := update[id].(map[string]any)
		if !ok {
			notUpdated[id] = jmap.InvalidProperties("the patch must be an object")
			continue
		}
		real, ok := c.ResolveID(id)
		if !ok {
			notUpdated[id] = jmap.NotFound()
			continue
		}
		t, prev, serr, derr := s.updateTask(c, tx, real, patch)
		if derr != nil {
			return nil, derr
		}
		if serr != nil {
			notUpdated[id] = serr
			continue
		}
		previous[id] = prev
		updated[id] = map[string]any{"updated": t.Data["updated"]}
	}

	for _, id := range destroy {
		real, ok := c.ResolveID(id)
		if !ok {
			notDestroyed[id] = jmap.NotFound()
			continue
		}
		t, derr := loadTask(tx, real)
		if derr != nil {
			return nil, derr
		}
		if t == nil {
			notDestroyed[id] = jmap.NotFound()
			continue
		}
		if derr := destroyTask(tx, real); derr != nil {
			return nil, derr
		}
		destroyed = append(destroyed, id)
	}

	// The spec requires every Task sharing a uid to agree on "privacy",
	// judged after the whole request has been applied so that a client
	// can change all instances at once.  Offending creates and updates
	// are unwound here, leaving the rest of the request in place.
	conflicts, derr := privacyConflicts(tx, createdTasks, previous)
	if derr != nil {
		return nil, derr
	}
	for cid, t := range createdTasks {
		if !conflicts[t.UID] {
			continue
		}
		if derr := eraseTask(tx, t.ID); derr != nil {
			return nil, derr
		}
		c.ForgetCreatedID(cid)
		delete(created, cid)
		notCreated[cid] = privacyConflictError()
	}
	for id, prev := range previous {
		cur, derr := loadTask(tx, prev.ID)
		if derr != nil {
			return nil, derr
		}
		if !conflicts[prev.UID] && (cur == nil || !conflicts[cur.UID]) {
			continue
		}
		if derr := s.writeTask(tx, prev, false); derr != nil {
			return nil, derr
		}
		delete(updated, id)
		notUpdated[id] = privacyConflictError()
	}

	newState, derr := tx.State(typeTask)
	if derr != nil {
		return nil, derr
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return map[string]any{
		"accountId":    s.cfg.AccountID,
		"oldState":     store.StateString(oldState),
		"newState":     store.StateString(newState),
		"created":      nilIfEmpty(created),
		"notCreated":   nilIfEmpty(notCreated),
		"updated":      nilIfEmpty(updated),
		"notUpdated":   nilIfEmpty(notUpdated),
		"destroyed":    destroyed,
		"notDestroyed": nilIfEmpty(notDestroyed),
	}, nil
}

func privacyConflictError() *jmap.SetError {
	return jmap.InvalidProperties("all tasks with the same uid must have the same privacy", "privacy")
}

// privacyConflicts reports which uids touched by this request now have
// live tasks disagreeing on "privacy" (absent meaning "public").
func privacyConflicts(tx *store.Tx, createdTasks map[string]*task, previous map[string]*task) (map[string]bool, error) {
	touched := map[string]bool{}
	for _, t := range createdTasks {
		touched[t.UID] = true
	}
	for _, prev := range previous {
		touched[prev.UID] = true
		if cur, err := loadTask(tx, prev.ID); err != nil {
			return nil, err
		} else if cur != nil {
			touched[cur.UID] = true
		}
	}

	conflicts := map[string]bool{}
	for uid := range touched {
		rows, err := tx.Query(`SELECT COUNT(DISTINCT COALESCE(json_extract(data, '$.privacy'), 'public'))
			FROM tasks WHERE uid = ? AND destroyed_modseq IS NULL`, uid)
		if err != nil {
			return nil, err
		}
		var n int
		if rows.Next() {
			err = rows.Scan(&n)
		}
		rows.Close()
		if err != nil {
			return nil, err
		}
		if n > 1 {
			conflicts[uid] = true
		}
	}
	return conflicts, nil
}

// eraseTask removes a task created earlier in the same request as though
// it had never existed.  Unlike destroyTask it leaves no tombstone.
func eraseTask(tx *store.Tx, id string) error {
	for _, q := range []string{
		`DELETE FROM task_lists WHERE task_id = ?`,
		`DELETE FROM tasks_fts WHERE task_id = ?`,
		`DELETE FROM tasks WHERE id = ?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) createTask(c *jmap.Call, tx *store.Tx, props map[string]any) (*task, *jmap.SetError, error) {
	obj := make(map[string]any, len(props))
	for k, v := range props {
		if v != nil {
			obj[k] = v
		}
	}
	for _, prop := range []string{"id", "baseTaskId"} {
		if _, ok := obj[prop]; ok {
			return nil, jmap.InvalidProperties(prop+" is server-set", prop), nil
		}
	}
	lists, serr, err := extractTaskListIDs(c, tx, obj)
	if err != nil || serr != nil {
		return nil, serr, err
	}

	if _, ok := obj["@type"]; !ok {
		obj["@type"] = "Task"
	}
	if _, ok := obj["version"]; !ok {
		obj["version"] = jscalendarVersion
	}
	if _, ok := obj["uid"]; !ok {
		obj["uid"] = newID("")
	}
	nowStr := now()
	if _, ok := obj["created"]; !ok {
		obj["created"] = nowStr
	}
	obj["updated"] = nowStr

	if serr := validateTask(obj); serr != nil {
		return nil, serr, nil
	}
	if created := obj["created"].(string); created > nowStr {
		obj["created"] = nowStr
	}

	t := &task{ID: newID("T-"), UID: obj["uid"].(string), Data: obj, Lists: lists}
	if rid, ok := obj["recurrenceId"].(string); ok {
		t.RecurrenceID = &rid
	}
	// A create that collides on uid is most often a retry whose first
	// response was lost, so tell the client which task it already made.
	existing, err := existingTaskWithUID(tx, t.UID, t.RecurrenceID, t.ID)
	if err != nil {
		return nil, nil, err
	}
	if existing != "" {
		return nil, jmap.AlreadyExists("a task with this uid and recurrenceId already exists", existing), nil
	}
	return t, nil, s.writeTask(tx, t, true)
}

// updateTask applies patch to the task and returns the new and previous
// versions.
func (s *Service) updateTask(c *jmap.Call, tx *store.Tx, id string, patch map[string]any) (*task, *task, *jmap.SetError, error) {
	prev, err := loadTask(tx, id)
	if err != nil {
		return nil, nil, nil, err
	}
	if prev == nil {
		return nil, nil, jmap.NotFound(), nil
	}

	obj := prev.toJMAP()
	if serr := jmap.ApplyPatch(obj, patch); serr != nil {
		return nil, nil, serr, nil
	}
	if obj["id"] != id {
		return nil, nil, jmap.InvalidProperties("id is immutable", "id"), nil
	}
	delete(obj, "id")
	if _, ok := obj["baseTaskId"]; ok {
		return nil, nil, jmap.InvalidProperties("baseTaskId is server-set", "baseTaskId"), nil
	}
	lists, serr, err := extractTaskListIDs(c, tx, obj)
	if err != nil || serr != nil {
		return nil, nil, serr, err
	}
	obj["updated"] = now()

	if serr := validateTask(obj); serr != nil {
		return nil, nil, serr, nil
	}

	t := &task{ID: id, UID: obj["uid"].(string), Data: obj, Lists: lists}
	if rid, ok := obj["recurrenceId"].(string); ok {
		t.RecurrenceID = &rid
	}
	existing, err := existingTaskWithUID(tx, t.UID, t.RecurrenceID, t.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	if existing != "" {
		return nil, nil, jmap.InvalidProperties("a task with this uid and recurrenceId already exists", "uid"), nil
	}
	return t, prev, nil, s.writeTask(tx, t, false)
}
