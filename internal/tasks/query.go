package tasks

import (
	"cmp"
	"database/sql"
	"strings"
	"time"

	"github.com/rjbs/gizmo/jmap"
	"github.com/rjbs/taskmaster/internal/store"
)

// ErrCannotCalculateOccurrences is the Task/query error for recurrence
// expansion, which this server does not perform.
const ErrCannotCalculateOccurrences = "cannotCalculateOccurrences"

// Task/query is evaluated in Go rather than SQL.  The spec resolves a
// zoneless task in whatever zone the querying client names, so the instants
// that date filters and sorts compare cannot be stored; and with a single
// user's tasks, loading the sortable columns of every live task is cheap.
// Only the full-text conditions and list memberships are looked up in SQL.
// -- claude, 2026-10-02

// queryRow is the sortable, filterable summary of one task.
type queryRow struct {
	id, uid                        string
	startLocal, dueLocal, timeZone *string
	progress                       string
	progressRank                   int64
	percentComplete, priority      *int64
	created, updated               string
	lists                          map[string]bool
	startInstant, dueInstant       *time.Time
}

// sortProperties lists the Comparator properties Task/query accepts.
var sortProperties = map[string]bool{
	"id": true, "uid": true, "due": true, "start": true,
	"priority": true, "progress": true, "percentComplete": true,
	"created": true, "updated": true,
}

type comparator struct {
	property  string
	ascending bool
}

type predicate func(*queryRow) bool

// loadQueryRows reads every live task's summary and resolves its start and
// due instants, using loc for tasks that have no zone of their own.
func loadQueryRows(tx *store.Tx, loc *time.Location) ([]*queryRow, error) {
	rows, err := tx.Query(`SELECT id, uid, start_local, due_local, time_zone, progress,
		progress_rank, percent_complete, priority, created, updated
		FROM tasks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	zones := map[string]*time.Location{}
	zone := func(name *string) *time.Location {
		if name == nil {
			return loc
		}
		if z, ok := zones[*name]; ok {
			return z
		}
		z, ok := loadZone(*name)
		if !ok {
			z = loc
		}
		zones[*name] = z
		return z
	}
	instant := func(local *string, z *time.Location) *time.Time {
		if local == nil {
			return nil
		}
		t, ok := parseLocalDateTime(*local)
		if !ok {
			return nil
		}
		u := inZone(t, z)
		return &u
	}

	byID := map[string]*queryRow{}
	var out []*queryRow
	for rows.Next() {
		r := &queryRow{lists: map[string]bool{}}
		var startLocal, dueLocal, tz sql.NullString
		var pct, pri sql.NullInt64
		if err := rows.Scan(&r.id, &r.uid, &startLocal, &dueLocal, &tz, &r.progress,
			&r.progressRank, &pct, &pri, &r.created, &r.updated); err != nil {
			return nil, err
		}
		r.startLocal, r.dueLocal, r.timeZone = nullString(startLocal), nullString(dueLocal), nullString(tz)
		r.percentComplete, r.priority = nullInt(pct), nullInt(pri)
		z := zone(r.timeZone)
		r.startInstant, r.dueInstant = instant(r.startLocal, z), instant(r.dueLocal, z)
		byID[r.id] = r
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	members, err := tx.Query(`SELECT task_id, list_id FROM task_lists`)
	if err != nil {
		return nil, err
	}
	defer members.Close()
	for members.Next() {
		var taskID, listID string
		if err := members.Scan(&taskID, &listID); err != nil {
			return nil, err
		}
		if r, ok := byID[taskID]; ok {
			r.lists[listID] = true
		}
	}
	return out, members.Err()
}

func nullString(ns sql.NullString) *string {
	if ns.Valid {
		return &ns.String
	}
	return nil
}

func nullInt(ni sql.NullInt64) *int64 {
	if ni.Valid {
		return &ni.Int64
	}
	return nil
}

// compileFilter turns a Filter (operator tree or single condition) into a
// predicate over query rows.
func compileFilter(c *jmap.Call, tx *store.Tx, f any, loc *time.Location) (predicate, *jmap.MethodError) {
	switch v := f.(type) {
	case nil:
		return func(*queryRow) bool { return true }, nil
	case map[string]any:
		if op, ok := v["operator"].(string); ok {
			return compileOperator(c, tx, op, v["conditions"], loc)
		}
		return compileCondition(c, tx, v, loc)
	default:
		return nil, &jmap.MethodError{Type: jmap.ErrUnsupportedFilter, Description: "filter must be an object"}
	}
}

func compileOperator(c *jmap.Call, tx *store.Tx, op string, conditions any, loc *time.Location) (predicate, *jmap.MethodError) {
	list, ok := conditions.([]any)
	if !ok {
		return nil, &jmap.MethodError{Type: jmap.ErrUnsupportedFilter, Description: "conditions must be an array"}
	}
	var children []predicate
	for _, cond := range list {
		p, err := compileFilter(c, tx, cond, loc)
		if err != nil {
			return nil, err
		}
		children = append(children, p)
	}
	all := func(r *queryRow) bool {
		for _, p := range children {
			if !p(r) {
				return false
			}
		}
		return true
	}
	switch op {
	case "AND":
		return all, nil
	case "NOT":
		return func(r *queryRow) bool { return !all(r) }, nil
	case "OR":
		return func(r *queryRow) bool {
			for _, p := range children {
				if p(r) {
					return true
				}
			}
			return false
		}, nil
	}
	return nil, &jmap.MethodError{Type: jmap.ErrUnsupportedFilter, Description: "unknown operator " + op}
}

func compileCondition(c *jmap.Call, tx *store.Tx, cond map[string]any, loc *time.Location) (predicate, *jmap.MethodError) {
	var parts []predicate
	invalid := func(key, msg string) (predicate, *jmap.MethodError) {
		return nil, jmap.ArgError("filter %s %s", key, msg)
	}

	for key, raw := range cond {
		switch key {
		case "inTaskList":
			id, ok := raw.(string)
			if !ok {
				return invalid(key, "must be an Id")
			}
			real, resolved := c.ResolveID(id)
			parts = append(parts, func(r *queryRow) bool { return resolved && r.lists[real] })

		case "after", "before":
			str, _ := raw.(string)
			local, ok := parseLocalDateTime(str)
			if !ok {
				return invalid(key, "must be a LocalDateTime")
			}
			bound := inZone(local, loc)
			// A task occupies the span from start to due; "after" and
			// "before" together select tasks whose span overlaps the
			// window, hence the swapped fallbacks.
			if key == "after" {
				parts = append(parts, func(r *queryRow) bool {
					t := cmp.Or(r.dueInstant, r.startInstant)
					return t != nil && !t.Before(bound)
				})
			} else {
				parts = append(parts, func(r *queryRow) bool {
					t := cmp.Or(r.startInstant, r.dueInstant)
					return t != nil && t.Before(bound)
				})
			}

		case "progress":
			list, ok := raw.([]any)
			if !ok {
				return invalid(key, "must be an array of strings")
			}
			wanted := map[string]bool{}
			for _, v := range list {
				s, ok := v.(string)
				if !ok {
					return invalid(key, "must be an array of strings")
				}
				wanted[s] = true
			}
			parts = append(parts, func(r *queryRow) bool { return wanted[r.progress] })

		case "uid":
			s, ok := raw.(string)
			if !ok {
				return invalid(key, "must be a string")
			}
			parts = append(parts, func(r *queryRow) bool { return r.uid == s })

		case "text", "title", "description":
			s, ok := raw.(string)
			if !ok {
				return invalid(key, "must be a string")
			}
			columns := []string{key}
			if key == "text" {
				columns = []string{"title", "description", "other"}
			}
			match, ok := ftsMatch(columns, s)
			if !ok {
				continue
			}
			hits, err := ftsHits(tx, match)
			if err != nil {
				return nil, &jmap.MethodError{Type: jmap.ErrServerFail, Description: err.Error()}
			}
			parts = append(parts, func(r *queryRow) bool { return hits[r.id] })

		default:
			return nil, &jmap.MethodError{Type: jmap.ErrUnsupportedFilter, Description: "unsupported filter condition " + key}
		}
	}

	return func(r *queryRow) bool {
		for _, p := range parts {
			if !p(r) {
				return false
			}
		}
		return true
	}, nil
}

// ftsHits runs an FTS5 MATCH and returns the ids of the tasks it finds.
func ftsHits(tx *store.Tx, match string) (map[string]bool, error) {
	rows, err := tx.Query(`SELECT task_id FROM tasks_fts WHERE tasks_fts MATCH ?`, match)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hits := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		hits[id] = true
	}
	return hits, rows.Err()
}

// parseSort validates the Comparator list.  With none given, tasks sort by
// creation time.
func parseSort(sort any) ([]comparator, *jmap.MethodError) {
	if sort == nil {
		return []comparator{{property: "created", ascending: true}}, nil
	}
	list, ok := sort.([]any)
	if !ok {
		return nil, jmap.ArgError("sort must be an array of Comparator objects")
	}
	var out []comparator
	for _, raw := range list {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, jmap.ArgError("sort must be an array of Comparator objects")
		}
		prop, _ := m["property"].(string)
		if !sortProperties[prop] {
			return nil, &jmap.MethodError{Type: jmap.ErrUnsupportedSort, Description: "cannot sort on " + prop}
		}
		asc := true
		if v, ok := m["isAscending"].(bool); ok {
			asc = v
		}
		out = append(out, comparator{property: prop, ascending: asc})
	}
	if len(out) == 0 {
		out = append(out, comparator{property: "created", ascending: true})
	}
	return out, nil
}

// compare orders two rows by one property.  Tasks lacking the property
// sort after all that have it, whatever the direction.
func (c comparator) compare(a, b *queryRow) int {
	switch c.property {
	case "due":
		return nullLastCompare(a.dueInstant, b.dueInstant, time.Time.Compare, c.direction)
	case "start":
		return nullLastCompare(a.startInstant, b.startInstant, time.Time.Compare, c.direction)
	case "priority":
		return nullLastCompare(a.priority, b.priority, cmp.Compare[int64], c.direction)
	case "percentComplete":
		return nullLastCompare(a.percentComplete, b.percentComplete, cmp.Compare[int64], c.direction)
	case "progress":
		return c.direction(cmp.Compare(a.progressRank, b.progressRank))
	case "id":
		return c.direction(strings.Compare(a.id, b.id))
	case "uid":
		return c.direction(strings.Compare(a.uid, b.uid))
	case "created":
		return c.direction(strings.Compare(a.created, b.created))
	case "updated":
		return c.direction(strings.Compare(a.updated, b.updated))
	}
	return 0
}

func (c comparator) direction(n int) int {
	if c.ascending {
		return n
	}
	return -n
}

func nullLastCompare[T any](a, b *T, compare func(T, T) int, direction func(int) int) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return direction(compare(*a, *b))
}

func (s *Service) taskQueryChanges(c *jmap.Call, args map[string]any) (map[string]any, error) {
	return nil, &jmap.MethodError{Type: jmap.ErrCannotCalculateChanges}
}
