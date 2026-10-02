package tasks

import (
	"slices"
	"strings"
	"time"

	"github.com/rjbs/taskmaster/internal/jmap"
	"github.com/rjbs/taskmaster/internal/store"
)

// ErrCannotCalculateOccurrences is the Task/query error for recurrence
// expansion, which this server does not perform.
const ErrCannotCalculateOccurrences = "cannotCalculateOccurrences"

// sortColumns maps the sortable Task properties to the denormalized columns
// that hold them.
var sortColumns = map[string]string{
	"due":             "t.due_utc",
	"start":           "t.start_utc",
	"uid":             "t.uid",
	"recurrenceId":    "t.recurrence_id",
	"priority":        "t.priority",
	"progress":        "t.progress_rank",
	"percentComplete": "t.percent_complete",
	"created":         "t.created",
	"updated":         "t.updated",
}

func (s *Service) taskQuery(c *jmap.Context, args map[string]any) (map[string]any, error) {
	if err := s.checkAccount(args); err != nil {
		return nil, err
	}
	expand, err := argBool(args, "expandRecurrences", false)
	if err != nil {
		return nil, err
	}
	if expand {
		return nil, jmap.Errorf(ErrCannotCalculateOccurrences, "this server does not expand recurrences")
	}
	loc := time.UTC
	if tzName, ok, err := argString(args, "timeZone"); err != nil {
		return nil, err
	} else if ok {
		if loc, ok = loadZone(tzName); !ok {
			return nil, jmap.ArgError("timeZone must be an IANA time zone id")
		}
	}
	position, _, err := argInt(args, "position")
	if err != nil {
		return nil, err
	}
	anchor, hasAnchor, err := argString(args, "anchor")
	if err != nil {
		return nil, err
	}
	anchorOffset, _, err := argInt(args, "anchorOffset")
	if err != nil {
		return nil, err
	}
	limit, hasLimit, err := argUint(args, "limit")
	if err != nil {
		return nil, err
	}
	calculateTotal, err := argBool(args, "calculateTotal", false)
	if err != nil {
		return nil, err
	}

	where, params, err := buildFilter(c, args["filter"], loc)
	if err != nil {
		return nil, err
	}
	orderBy, err := buildSort(args["sort"])
	if err != nil {
		return nil, err
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
	rows, derr := tx.Query(`SELECT t.id FROM tasks t WHERE t.destroyed_modseq IS NULL AND (`+where+`) ORDER BY `+orderBy, params...)
	if derr != nil {
		return nil, derr
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	total := int64(len(ids))
	if hasAnchor {
		real, _ := c.ResolveID(anchor)
		idx := slices.Index(ids, real)
		if idx < 0 {
			return nil, &jmap.MethodError{Type: jmap.ErrAnchorNotFound}
		}
		position = int64(idx) + anchorOffset
	}
	if position < 0 {
		position = max(total+position, 0)
	}
	position = min(position, total)
	end := total
	if hasLimit {
		end = min(total, position+limit)
	}

	resp := map[string]any{
		"accountId":           s.cfg.AccountID,
		"queryState":          store.StateString(state),
		"canCalculateChanges": false,
		"position":            position,
		"ids":                 ids[position:end],
	}
	if calculateTotal {
		resp["total"] = total
	}
	return resp, nil
}

// buildFilter turns a Filter (operator tree or single condition) into a
// SQL expression over the tasks table aliased as t.
func buildFilter(c *jmap.Context, f any, loc *time.Location) (string, []any, *jmap.MethodError) {
	switch v := f.(type) {
	case nil:
		return "1", nil, nil
	case map[string]any:
		if op, ok := v["operator"].(string); ok {
			return buildOperator(c, op, v["conditions"], loc)
		}
		return buildCondition(c, v, loc)
	default:
		return "", nil, &jmap.MethodError{Type: jmap.ErrUnsupportedFilter, Description: "filter must be an object"}
	}
}

func buildOperator(c *jmap.Context, op string, conditions any, loc *time.Location) (string, []any, *jmap.MethodError) {
	list, ok := conditions.([]any)
	if !ok {
		return "", nil, &jmap.MethodError{Type: jmap.ErrUnsupportedFilter, Description: "conditions must be an array"}
	}
	var parts []string
	var params []any
	for _, cond := range list {
		sql, p, err := buildFilter(c, cond, loc)
		if err != nil {
			return "", nil, err
		}
		parts = append(parts, "("+sql+")")
		params = append(params, p...)
	}
	switch op {
	case "AND":
		if len(parts) == 0 {
			return "1", nil, nil
		}
		return strings.Join(parts, " AND "), params, nil
	case "OR":
		if len(parts) == 0 {
			return "0", nil, nil
		}
		return strings.Join(parts, " OR "), params, nil
	case "NOT":
		if len(parts) == 0 {
			return "1", nil, nil
		}
		return "NOT (" + strings.Join(parts, " AND ") + ")", params, nil
	}
	return "", nil, &jmap.MethodError{Type: jmap.ErrUnsupportedFilter, Description: "unknown operator " + op}
}

func buildCondition(c *jmap.Context, cond map[string]any, loc *time.Location) (string, []any, *jmap.MethodError) {
	var parts []string
	var params []any
	add := func(sql string, p ...any) {
		parts = append(parts, sql)
		params = append(params, p...)
	}
	unsupported := func(key string) (string, []any, *jmap.MethodError) {
		return "", nil, &jmap.MethodError{Type: jmap.ErrUnsupportedFilter, Description: "unsupported filter condition " + key}
	}
	invalid := func(key, msg string) (string, []any, *jmap.MethodError) {
		return "", nil, jmap.ArgError("filter %s %s", key, msg)
	}

	for key, raw := range cond {
		switch key {
		case "inTaskList":
			id, ok := raw.(string)
			if !ok {
				return invalid(key, "must be an Id")
			}
			real, ok := c.ResolveID(id)
			if !ok {
				add("0")
				continue
			}
			add("t.id IN (SELECT task_id FROM task_lists WHERE list_id = ?)", real)

		case "after", "before":
			str, _ := raw.(string)
			local, ok := parseLocalDateTime(str)
			if !ok {
				return invalid(key, "must be a LocalDateTime")
			}
			utc := formatUTC(inZone(local, loc))
			// A task occupies the span from start to due; "after" and
			// "before" together select tasks whose span overlaps the
			// window, hence the swapped fallbacks.
			if key == "after" {
				add("COALESCE(t.due_utc, t.start_utc) >= ?", utc)
			} else {
				add("COALESCE(t.start_utc, t.due_utc) < ?", utc)
			}

		case "progress":
			list, ok := raw.([]any)
			if !ok {
				return invalid(key, "must be an array of strings")
			}
			if len(list) == 0 {
				add("0")
				continue
			}
			marks := make([]string, 0, len(list))
			for _, v := range list {
				s, ok := v.(string)
				if !ok {
					return invalid(key, "must be an array of strings")
				}
				marks = append(marks, "?")
				params = append(params, s)
			}
			parts = append(parts, "t.progress IN ("+strings.Join(marks, ", ")+")")

		case "uid":
			s, ok := raw.(string)
			if !ok {
				return invalid(key, "must be a string")
			}
			add("t.uid = ?", s)

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
			add("t.id IN (SELECT task_id FROM tasks_fts WHERE tasks_fts MATCH ?)", match)

		default:
			return unsupported(key)
		}
	}
	if len(parts) == 0 {
		return "1", nil, nil
	}
	return strings.Join(parts, " AND "), params, nil
}

// buildSort turns the Comparator list into an ORDER BY clause.  Tasks
// missing the sort property go last regardless of direction, and ties
// break on id so paging is stable.
func buildSort(sort any) (string, *jmap.MethodError) {
	var parts []string
	if sort != nil {
		list, ok := sort.([]any)
		if !ok {
			return "", jmap.ArgError("sort must be an array of Comparator objects")
		}
		for _, raw := range list {
			cmp, ok := raw.(map[string]any)
			if !ok {
				return "", jmap.ArgError("sort must be an array of Comparator objects")
			}
			prop, _ := cmp["property"].(string)
			col, ok := sortColumns[prop]
			if !ok {
				return "", &jmap.MethodError{Type: jmap.ErrUnsupportedSort, Description: "cannot sort on " + prop}
			}
			dir := "ASC"
			if asc, ok := cmp["isAscending"].(bool); ok && !asc {
				dir = "DESC"
			}
			parts = append(parts, "("+col+" IS NULL) ASC, "+col+" "+dir)
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "t.created ASC")
	}
	parts = append(parts, "t.id ASC")
	return strings.Join(parts, ", "), nil
}

func (s *Service) taskQueryChanges(c *jmap.Context, args map[string]any) (map[string]any, error) {
	if err := s.checkAccount(args); err != nil {
		return nil, err
	}
	return nil, &jmap.MethodError{Type: jmap.ErrCannotCalculateChanges}
}
