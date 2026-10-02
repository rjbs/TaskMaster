package tasks

import (
	"database/sql"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/rjbs/taskmaster/internal/jmap"
	"github.com/rjbs/taskmaster/internal/store"
)

// SetErrTaskListHasTask is the SetError type for destroying a non-empty
// list without onDestroyRemoveTasks.
const SetErrTaskListHasTask = "taskListHasTask"

type taskList struct {
	ID                string
	Name              string
	Description       *string
	Color             *string
	SortOrder         int64
	IsSubscribed      bool
	IsVisible         bool
	IsDefault         bool
	AlertsWithTime    *string // JSON text, or nil for null
	AlertsWithoutTime *string
	TimeZone          *string
}

// rights is the TaskListRights object for the one user, who may do
// everything except share, since there is nobody to share with.
func rights() map[string]any {
	return map[string]any{
		"mayReadItems":     true,
		"mayWrite":         true,
		"mayUpdatePrivate": true,
		"mayShare":         false,
		"mayDelete":        true,
	}
}

var taskListProps = map[string]bool{
	"id": true, "name": true, "description": true, "color": true,
	"sortOrder": true, "isSubscribed": true, "isVisible": true,
	"isDefault": true, "defaultAlertsWithTime": true,
	"defaultAlertsWithoutTime": true, "timeZone": true, "shareWith": true,
	"myRights": true,
}

// taskListDefaults gives the value each optional property takes when
// omitted on create or set to null on update.
func taskListDefaults() map[string]any {
	return map[string]any{
		"description":              nil,
		"color":                    nil,
		"sortOrder":                json.Number("0"),
		"isSubscribed":             true,
		"isVisible":                true,
		"isDefault":                false,
		"defaultAlertsWithTime":    nil,
		"defaultAlertsWithoutTime": nil,
		"timeZone":                 nil,
		"shareWith":                nil,
	}
}

func strOrNil(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func jsonOrNil(p *string) any {
	if p == nil {
		return nil
	}
	v, err := jmap.DecodeValue([]byte(*p))
	if err != nil {
		return nil
	}
	return v
}

func (l *taskList) toJMAP() map[string]any {
	return map[string]any{
		"id":                       l.ID,
		"name":                     l.Name,
		"description":              strOrNil(l.Description),
		"color":                    strOrNil(l.Color),
		"sortOrder":                json.Number(store.StateString(l.SortOrder)),
		"isSubscribed":             l.IsSubscribed,
		"isVisible":                l.IsVisible,
		"isDefault":                l.IsDefault,
		"defaultAlertsWithTime":    jsonOrNil(l.AlertsWithTime),
		"defaultAlertsWithoutTime": jsonOrNil(l.AlertsWithoutTime),
		"timeZone":                 strOrNil(l.TimeZone),
		"shareWith":                nil,
		"myRights":                 rights(),
	}
}

// taskListFromMap validates a complete TaskList representation (all
// properties present, defaults already applied) and builds the row.
func taskListFromMap(m map[string]any) (*taskList, *jmap.SetError) {
	var bad, why []string
	fail := func(prop, msg string) {
		bad = append(bad, prop)
		why = append(why, prop+": "+msg)
	}
	l := &taskList{}

	for k := range m {
		if !taskListProps[k] {
			fail(k, "unknown property")
		}
	}

	if id, ok := m["id"].(string); ok {
		l.ID = id
	}

	if name, ok := m["name"].(string); ok && name != "" && len(name) <= 255 {
		l.Name = name
	} else {
		fail("name", "must be a non-empty string of at most 255 octets")
	}

	optString := func(prop string, check func(string) bool, msg string) *string {
		switch v := m[prop].(type) {
		case nil:
			return nil
		case string:
			if check != nil && !check(v) {
				fail(prop, msg)
				return nil
			}
			return &v
		default:
			fail(prop, "must be a string or null")
			return nil
		}
	}
	l.Description = optString("description", nil, "")
	l.Color = optString("color", validColor, "must be a CSS3 color keyword or hex RGB value")
	l.TimeZone = optString("timeZone", func(s string) bool { _, ok := loadZone(s); return ok }, "must be an IANA time zone id")

	if n, ok := intValue(m["sortOrder"]); ok && n >= 0 && n < 1<<31 {
		l.SortOrder = n
	} else {
		fail("sortOrder", "must be an integer in the range 0 <= sortOrder < 2^31")
	}

	optBool := func(prop string) bool {
		b, ok := m[prop].(bool)
		if !ok {
			fail(prop, "must be a boolean")
		}
		return b
	}
	l.IsSubscribed = optBool("isSubscribed")
	l.IsVisible = optBool("isVisible")
	l.IsDefault = optBool("isDefault")

	for prop, dst := range map[string]**string{
		"defaultAlertsWithTime":    &l.AlertsWithTime,
		"defaultAlertsWithoutTime": &l.AlertsWithoutTime,
	} {
		if m[prop] == nil {
			continue
		}
		text, msg := validateDefaultAlerts(m[prop])
		if msg != "" {
			fail(prop, msg)
			continue
		}
		*dst = &text
	}

	if m["shareWith"] != nil {
		fail("shareWith", "sharing is not supported by this server")
	}
	if r, ok := m["myRights"]; ok && !reflect.DeepEqual(r, rights()) {
		fail("myRights", "is server-set")
	}

	if len(bad) > 0 {
		return nil, jmap.InvalidProperties(strings.Join(why, "; "), bad...)
	}
	return l, nil
}

// validateDefaultAlerts checks an Id[Alert] map and returns it re-encoded,
// or a reason it is unacceptable.
func validateDefaultAlerts(v any) (string, string) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", "must be an object or null"
	}
	for id, a := range m {
		if !jmap.ValidID(id) {
			return "", "alert id " + id + " is not a valid Id"
		}
		alert, ok := a.(map[string]any)
		if !ok {
			return "", "alert " + id + " must be an object"
		}
		if t, ok := alert["@type"].(string); ok && t != "Alert" {
			return "", "alert " + id + " has @type " + t
		}
		if trig, ok := alert["trigger"].(map[string]any); ok {
			if tt, _ := trig["@type"].(string); tt == "AbsoluteTrigger" {
				return "", "default alert " + id + " must not use an AbsoluteTrigger"
			}
		}
	}
	text, err := encodeJSON(m)
	if err != nil {
		return "", err.Error()
	}
	return text, ""
}

const taskListColumns = `id, name, description, color, sort_order, is_subscribed,
	is_visible, is_default, default_alerts_with_time, default_alerts_without_time, time_zone`

func scanTaskList(sc interface{ Scan(...any) error }) (*taskList, error) {
	l := &taskList{}
	var desc, color, withTime, withoutTime, tz sql.NullString
	err := sc.Scan(&l.ID, &l.Name, &desc, &color, &l.SortOrder, &l.IsSubscribed,
		&l.IsVisible, &l.IsDefault, &withTime, &withoutTime, &tz)
	if err != nil {
		return nil, err
	}
	nullable := func(ns sql.NullString) *string {
		if ns.Valid {
			return &ns.String
		}
		return nil
	}
	l.Description, l.Color, l.TimeZone = nullable(desc), nullable(color), nullable(tz)
	l.AlertsWithTime, l.AlertsWithoutTime = nullable(withTime), nullable(withoutTime)
	return l, nil
}

func loadTaskList(tx *store.Tx, id string) (*taskList, error) {
	row := tx.QueryRow(`SELECT `+taskListColumns+` FROM tasklists WHERE id = ? AND destroyed_modseq IS NULL`, id)
	l, err := scanTaskList(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return l, err
}

func loadAllTaskLists(tx *store.Tx) ([]*taskList, error) {
	rows, err := tx.Query(`SELECT ` + taskListColumns + ` FROM tasklists
		WHERE destroyed_modseq IS NULL ORDER BY sort_order, name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lists []*taskList
	for rows.Next() {
		l, err := scanTaskList(rows)
		if err != nil {
			return nil, err
		}
		lists = append(lists, l)
	}
	return lists, rows.Err()
}

func insertTaskList(tx *store.Tx, l *taskList) error {
	modseq, err := tx.NextModSeq(typeTaskList)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO tasklists (`+taskListColumns+`, created_modseq, updated_modseq)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.Name, l.Description, l.Color, l.SortOrder, l.IsSubscribed, l.IsVisible,
		l.IsDefault, l.AlertsWithTime, l.AlertsWithoutTime, l.TimeZone, modseq, modseq)
	return err
}

func updateTaskListRow(tx *store.Tx, l *taskList) error {
	modseq, err := tx.NextModSeq(typeTaskList)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE tasklists SET name = ?, description = ?, color = ?, sort_order = ?,
		is_subscribed = ?, is_visible = ?, is_default = ?, default_alerts_with_time = ?,
		default_alerts_without_time = ?, time_zone = ?, updated_modseq = ? WHERE id = ?`,
		l.Name, l.Description, l.Color, l.SortOrder, l.IsSubscribed, l.IsVisible, l.IsDefault,
		l.AlertsWithTime, l.AlertsWithoutTime, l.TimeZone, modseq, l.ID)
	return err
}

func (s *Service) taskListGet(c *jmap.Context, args map[string]any) (map[string]any, error) {
	if err := s.checkAccount(args); err != nil {
		return nil, err
	}
	ids, hasIDs, err := argStringList(args, "ids")
	if err != nil {
		return nil, err
	}
	props, hasProps, err := argStringList(args, "properties")
	if err != nil {
		return nil, err
	}
	for _, p := range props {
		if !taskListProps[p] {
			return nil, jmap.ArgError("unknown property %q", p)
		}
	}

	tx, derr := s.begin(c)
	if derr != nil {
		return nil, derr
	}
	defer tx.Rollback()

	state, derr := tx.State(typeTaskList)
	if derr != nil {
		return nil, derr
	}

	var lists []*taskList
	notFound := []string{}
	if !hasIDs {
		if lists, derr = loadAllTaskLists(tx); derr != nil {
			return nil, derr
		}
	} else {
		for _, id := range ids {
			real, ok := c.ResolveID(id)
			if !ok {
				notFound = append(notFound, id)
				continue
			}
			l, derr := loadTaskList(tx, real)
			if derr != nil {
				return nil, derr
			}
			if l == nil {
				notFound = append(notFound, id)
				continue
			}
			lists = append(lists, l)
		}
	}

	list := make([]any, 0, len(lists))
	for _, l := range lists {
		list = append(list, filterProperties(l.toJMAP(), props, hasProps))
	}
	return map[string]any{
		"accountId": s.cfg.AccountID,
		"state":     store.StateString(state),
		"list":      list,
		"notFound":  notFound,
	}, nil
}

func (s *Service) taskListChanges(c *jmap.Context, args map[string]any) (map[string]any, error) {
	if err := s.checkAccount(args); err != nil {
		return nil, err
	}
	tx, err := s.begin(c)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return s.changes(tx, "tasklists", typeTaskList, args)
}

func (s *Service) taskListSet(c *jmap.Context, args map[string]any) (map[string]any, error) {
	if err := s.checkAccount(args); err != nil {
		return nil, err
	}
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
	removeTasks, err := argBool(args, "onDestroyRemoveTasks", false)
	if err != nil {
		return nil, err
	}
	setDefault, hasSetDefault, err := argString(args, "onSuccessSetIsDefault")
	if err != nil {
		return nil, err
	}

	tx, derr := s.begin(c)
	if derr != nil {
		return nil, derr
	}
	defer tx.Rollback()

	oldState, derr := tx.State(typeTaskList)
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

	for _, cid := range slices.Sorted(maps.Keys(create)) {
		props, ok := create[cid].(map[string]any)
		if !ok {
			notCreated[cid] = jmap.InvalidProperties("the value to create must be an object")
			continue
		}
		l, serr, derr := createTaskList(tx, props)
		if derr != nil {
			return nil, derr
		}
		if serr != nil {
			notCreated[cid] = serr
			continue
		}
		c.CreatedIDs[cid] = l.ID
		created[cid] = map[string]any{
			"id":        l.ID,
			"isDefault": false,
			"myRights":  rights(),
			"shareWith": nil,
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
		serr, derr := updateTaskList(tx, real, patch)
		if derr != nil {
			return nil, derr
		}
		if serr != nil {
			notUpdated[id] = serr
			continue
		}
		updated[id] = nil
	}

	for _, id := range destroy {
		real, ok := c.ResolveID(id)
		if !ok {
			notDestroyed[id] = jmap.NotFound()
			continue
		}
		serr, derr := s.destroyTaskList(tx, real, removeTasks)
		if derr != nil {
			return nil, derr
		}
		if serr != nil {
			notDestroyed[id] = serr
			continue
		}
		destroyed = append(destroyed, id)
	}

	allSucceeded := len(notCreated)+len(notUpdated)+len(notDestroyed) == 0
	if allSucceeded && hasSetDefault {
		if real, ok := c.ResolveID(setDefault); ok {
			changed, prev, derr := setDefaultTaskList(tx, real)
			if derr != nil {
				return nil, derr
			}
			if changed {
				reported := false
				for _, v := range created {
					if m := v.(map[string]any); m["id"] == real {
						m["isDefault"] = true
						reported = true
					}
				}
				if !reported {
					updated[real] = map[string]any{"isDefault": true}
				}
				if prev != "" {
					updated[prev] = map[string]any{"isDefault": false}
				}
			}
		}
	}

	newState, derr := tx.State(typeTaskList)
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

func createTaskList(tx *store.Tx, props map[string]any) (*taskList, *jmap.SetError, error) {
	if _, ok := props["id"]; ok {
		return nil, jmap.InvalidProperties("id is server-set", "id"), nil
	}
	if isDefault, _ := props["isDefault"].(bool); isDefault {
		return nil, jmap.InvalidProperties("isDefault is server-set; use onSuccessSetIsDefault", "isDefault"), nil
	}
	m := taskListDefaults()
	for k, v := range props {
		if v != nil {
			m[k] = v
		}
	}
	l, serr := taskListFromMap(m)
	if serr != nil {
		return nil, serr, nil
	}
	l.ID = newID("L-")
	return l, nil, insertTaskList(tx, l)
}

func updateTaskList(tx *store.Tx, id string, patch map[string]any) (*jmap.SetError, error) {
	cur, err := loadTaskList(tx, id)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return jmap.NotFound(), nil
	}
	m := cur.toJMAP()
	if serr := jmap.ApplyPatch(m, patch); serr != nil {
		return serr, nil
	}
	for k, def := range taskListDefaults() {
		if _, ok := m[k]; !ok {
			m[k] = def
		}
	}
	if m["id"] != cur.ID {
		return jmap.InvalidProperties("id is immutable", "id"), nil
	}
	if isDefault, _ := m["isDefault"].(bool); isDefault != cur.IsDefault {
		return jmap.InvalidProperties("isDefault is server-set; use onSuccessSetIsDefault", "isDefault"), nil
	}
	l, serr := taskListFromMap(m)
	if serr != nil {
		return serr, nil
	}
	l.ID = id
	return nil, updateTaskListRow(tx, l)
}

func (s *Service) destroyTaskList(tx *store.Tx, id string, removeTasks bool) (*jmap.SetError, error) {
	cur, err := loadTaskList(tx, id)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return jmap.NotFound(), nil
	}

	rows, err := tx.Query(`SELECT task_id FROM task_lists WHERE list_id = ?`, id)
	if err != nil {
		return nil, err
	}
	var taskIDs []string
	for rows.Next() {
		var tid string
		if err := rows.Scan(&tid); err != nil {
			rows.Close()
			return nil, err
		}
		taskIDs = append(taskIDs, tid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(taskIDs) > 0 && !removeTasks {
		return &jmap.SetError{
			Type:        SetErrTaskListHasTask,
			Description: "the task list still contains tasks; pass onDestroyRemoveTasks to remove them",
		}, nil
	}

	for _, tid := range taskIDs {
		if _, err := tx.Exec(`DELETE FROM task_lists WHERE task_id = ? AND list_id = ?`, tid, id); err != nil {
			return nil, err
		}
		var remaining int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM task_lists WHERE task_id = ?`, tid).Scan(&remaining); err != nil {
			return nil, err
		}
		if remaining == 0 {
			err = destroyTask(tx, tid)
		} else {
			err = touchTask(tx, tid)
		}
		if err != nil {
			return nil, err
		}
	}

	modseq, err := tx.NextModSeq(typeTaskList)
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(`UPDATE tasklists SET destroyed_modseq = ?, updated_modseq = ? WHERE id = ?`, modseq, modseq, id)
	return nil, err
}

// setDefaultTaskList makes id the default list.  It reports whether
// anything changed and which list, if any, was the default before.
func setDefaultTaskList(tx *store.Tx, id string) (changed bool, prev string, err error) {
	l, err := loadTaskList(tx, id)
	if err != nil || l == nil || l.IsDefault {
		return false, "", err
	}

	err = tx.QueryRow(`SELECT id FROM tasklists WHERE is_default AND destroyed_modseq IS NULL`).Scan(&prev)
	if err != nil && err != sql.ErrNoRows {
		return false, "", err
	}
	if prev != "" {
		modseq, err := tx.NextModSeq(typeTaskList)
		if err != nil {
			return false, "", err
		}
		if _, err := tx.Exec(`UPDATE tasklists SET is_default = 0, updated_modseq = ? WHERE id = ?`, modseq, prev); err != nil {
			return false, "", err
		}
	}
	modseq, err := tx.NextModSeq(typeTaskList)
	if err != nil {
		return false, "", err
	}
	_, err = tx.Exec(`UPDATE tasklists SET is_default = 1, updated_modseq = ? WHERE id = ?`, modseq, id)
	return err == nil, prev, err
}
