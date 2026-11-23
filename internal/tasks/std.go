package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rjbs/gizmo/jmap"
	"github.com/rjbs/gizmo/std"

	"github.com/rjbs/taskmaster/internal/store"
)

// This file is where the standard methods of TaskList and Task meet
// Gizmo's drivers for them.  The drivers see to the arguments, the
// limits, paging, ifInState, creation ids and the shape of the response.
// What is here is what only this package knows: how to load a task or a
// list, check it, write it and find it.

// txn is a store transaction as Gizmo's drivers know one.
type txn struct {
	*store.Tx
}

func (s *Service) beginTxn(ctx context.Context, _ bool) (*txn, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &txn{tx}, nil
}

// State gives a type's state as a string.  It hides the store's State,
// which gives it as a number; that one is still there as t.Tx.State.
func (t *txn) State(_ context.Context, typ string) (string, error) {
	n, err := t.Tx.State(typ)
	return store.StateString(n), err
}

func (t *txn) Savepoint(_ context.Context, fn func() error) error {
	return t.Tx.Savepoint(fn)
}

// limits are the limits the standard methods keep to, which are the ones
// the server advertises.
var limits = std.Limits{MaxObjectsInGet: MaxObjectsInGet, MaxObjectsInSet: MaxObjectsInSet, MaxQueryLimit: MaxObjectsInGet}

// The most objects one /get may return, and one /set may change.  A task
// list is small, but someone's tasks, with the finished ones, need not be.
const (
	MaxObjectsInGet = 10000
	MaxObjectsInSet = 1000
)

// failure makes one error of the two that this package's functions return
// for an object: a SetError, which is the object's own failure, or any
// other, which is the method's.
func failure(serr *jmap.SetError, err error) error {
	if err != nil {
		return err
	}
	if serr != nil {
		return serr
	}
	return nil
}

// ---- TaskList -----------------------------------------------------------

func (s *Service) taskListGet(c *jmap.Call, raw json.RawMessage) (any, error) {
	req, err := std.ParseGet(raw, nil)
	if err != nil {
		return nil, err
	}
	return std.Get(c, s.beginTxn, limits, std.Getter[*txn]{
		Type: typeTaskList,
		KnownProperty: func(_ context.Context, name string) bool {
			return taskListProps[name] || isVendorProp(name)
		},
		Load: func(_ context.Context, tx *txn, ids, _ []string) (map[string]map[string]any, []string, error) {
			var lists []*taskList
			if ids == nil {
				all, err := loadAllTaskLists(tx.Tx)
				if err != nil {
					return nil, nil, err
				}
				lists = all
			}
			for _, id := range ids {
				l, err := loadTaskList(tx.Tx, id)
				if err != nil {
					return nil, nil, err
				}
				if l != nil {
					lists = append(lists, l)
				}
			}
			objects, found := map[string]map[string]any{}, []string{}
			for _, l := range lists {
				objects[l.ID] = l.toJMAP()
				found = append(found, l.ID)
			}
			return objects, found, nil
		},
	}, req)
}

func (s *Service) taskListChanges(c *jmap.Call, raw json.RawMessage) (any, error) {
	return s.changes(c, raw, "tasklists", typeTaskList)
}

func (s *Service) taskListSet(c *jmap.Call, raw json.RawMessage) (any, error) {
	var extra struct {
		OnDestroyRemoveTasks  bool    `json:"onDestroyRemoveTasks"`
		OnSuccessSetIsDefault *string `json:"onSuccessSetIsDefault"`
	}
	req, err := std.ParseSet(raw, &extra)
	if err != nil {
		return nil, err
	}
	return std.Set(c, s.beginTxn, limits, std.Setter[*txn]{
		Type: typeTaskList,
		Create: func(_ context.Context, tx *txn, raw json.RawMessage) (string, map[string]any, error) {
			props, err := jmap.DecodeObject(raw)
			if err != nil {
				return "", nil, err
			}
			l, serr, err := createTaskList(tx.Tx, props)
			if err := failure(serr, err); err != nil {
				return "", nil, err
			}
			return l.ID, map[string]any{"isDefault": false, "myRights": rights(), "shareWith": nil}, nil
		},
		Update: func(_ context.Context, tx *txn, id string, raw json.RawMessage) (map[string]any, error) {
			patch, err := jmap.DecodeObject(raw)
			if err != nil {
				return nil, err
			}
			return nil, failure(updateTaskList(tx.Tx, id, patch))
		},
		Destroy: func(_ context.Context, tx *txn, id string) error {
			return failure(s.destroyTaskList(tx.Tx, id, extra.OnDestroyRemoveTasks))
		},
		Finish: func(_ context.Context, tx *txn, r *std.SetResult) error {
			if extra.OnSuccessSetIsDefault == nil || len(r.NotCreated)+len(r.NotUpdated)+len(r.NotDestroyed) > 0 {
				return nil
			}
			id, ok := c.ResolveID(*extra.OnSuccessSetIsDefault)
			if !ok {
				return nil
			}
			changed, prev, err := setDefaultTaskList(tx.Tx, id)
			if err != nil || !changed {
				return err
			}
			reported := false
			for _, told := range r.Created {
				if told["id"] == id {
					told["isDefault"] = true
					reported = true
				}
			}
			if !reported {
				r.Updated[id] = map[string]any{"isDefault": true}
			}
			if prev != "" {
				r.Updated[prev] = map[string]any{"isDefault": false}
			}
			return nil
		},
	}, req)
}

// ---- Task ---------------------------------------------------------------

func (s *Service) taskGet(c *jmap.Call, raw json.RawMessage) (any, error) {
	var extra struct {
		After  *string `json:"recurrenceOverridesAfter"`
		Before *string `json:"recurrenceOverridesBefore"`
	}
	req, err := std.ParseGet(raw, &extra)
	if err != nil {
		return nil, err
	}
	for name, v := range map[string]*string{"recurrenceOverridesAfter": extra.After, "recurrenceOverridesBefore": extra.Before} {
		if v != nil {
			if _, valid := parseUTCDateTime(*v); !valid {
				return nil, jmap.ArgError("%s must be a UTCDateTime", name)
			}
		}
	}
	return std.Get(c, s.beginTxn, limits, std.Getter[*txn]{
		Type: typeTask,
		Count: func(_ context.Context, tx *txn) (int, error) {
			var n int
			err := tx.QueryRow(`SELECT COUNT(*) FROM tasks WHERE destroyed_modseq IS NULL`).Scan(&n)
			return n, err
		},
		Load: func(_ context.Context, tx *txn, ids, props []string) (map[string]map[string]any, []string, error) {
			if ids == nil {
				all, err := allTaskIDs(tx.Tx)
				if err != nil {
					return nil, nil, err
				}
				ids = all
			}
			objects, found := map[string]map[string]any{}, []string{}
			for _, id := range ids {
				t, err := loadTask(tx.Tx, id)
				if err != nil {
					return nil, nil, err
				}
				if t == nil {
					continue
				}
				obj := t.toJMAP()
				s.windowOverrides(obj, extra.After, extra.Before)
				if _, ok := obj["useDefaultAlerts"]; !ok {
					obj["useDefaultAlerts"] = false
				}
				// iCalendar is returned only when asked for by name.
				if props == nil {
					delete(obj, "iCalendar")
				}
				objects[id] = obj
				found = append(found, id)
			}
			return objects, found, nil
		},
	}, req)
}

func (s *Service) taskChanges(c *jmap.Call, raw json.RawMessage) (any, error) {
	return s.changes(c, raw, "tasks", typeTask)
}

func (s *Service) taskSet(c *jmap.Call, raw json.RawMessage) (any, error) {
	req, err := std.ParseSet(raw, nil)
	if err != nil {
		return nil, err
	}

	// Remembered so that a privacy conflict found after everything has
	// been applied can be unwound for just the tasks involved.
	createdTasks := map[string]*task{} // by id
	previous := map[string]*task{}     // by id

	return std.Set(c, s.beginTxn, limits, std.Setter[*txn]{
		Type: typeTask,
		Create: func(_ context.Context, tx *txn, raw json.RawMessage) (string, map[string]any, error) {
			props, err := jmap.DecodeObject(raw)
			if err != nil {
				return "", nil, err
			}
			t, serr, err := s.createTask(c, tx.Tx, props)
			if err := failure(serr, err); err != nil {
				return "", nil, err
			}
			createdTasks[t.ID] = t
			return t.ID, map[string]any{
				"@type":   "Task",
				"version": t.Data["version"],
				"uid":     t.UID,
				"created": t.Data["created"],
				"updated": t.Data["updated"],
			}, nil
		},
		Update: func(_ context.Context, tx *txn, id string, raw json.RawMessage) (map[string]any, error) {
			patch, err := jmap.DecodeObject(raw)
			if err != nil {
				return nil, err
			}
			t, prev, serr, err := s.updateTask(c, tx.Tx, id, patch)
			if err := failure(serr, err); err != nil {
				return nil, err
			}
			// If the task was also made in this call, its creation is
			// what a conflict unwinds, and it has no earlier self.
			if _, made := createdTasks[id]; !made {
				if _, seen := previous[id]; !seen {
					previous[id] = prev
				}
			}
			return map[string]any{"updated": t.Data["updated"]}, nil
		},
		Destroy: func(_ context.Context, tx *txn, id string) error {
			t, err := loadTask(tx.Tx, id)
			if err != nil {
				return err
			}
			if t == nil {
				return jmap.NotFound()
			}
			delete(createdTasks, id)
			delete(previous, id)
			return destroyTask(tx.Tx, id)
		},

		// The spec requires every Task sharing a uid to agree on
		// "privacy", judged after the whole request has been applied so
		// that a client can change all instances at once.  Offending
		// creates and updates are unwound here, leaving the rest of the
		// request in place.
		Finish: func(_ context.Context, tx *txn, r *std.SetResult) error {
			conflicts, err := privacyConflicts(tx.Tx, createdTasks, previous)
			if err != nil {
				return err
			}
			for cid, told := range r.Created {
				t := createdTasks[told["id"].(string)]
				if t == nil || !conflicts[t.UID] {
					continue
				}
				if err := eraseTask(tx.Tx, t.ID); err != nil {
					return err
				}
				delete(r.Created, cid)
				delete(r.Updated, t.ID)
				r.NotCreated[cid] = privacyConflictError()
			}
			for id, prev := range previous {
				cur, err := loadTask(tx.Tx, prev.ID)
				if err != nil {
					return err
				}
				if !conflicts[prev.UID] && (cur == nil || !conflicts[cur.UID]) {
					continue
				}
				if err := s.writeTask(tx.Tx, prev, false); err != nil {
					return err
				}
				delete(r.Updated, id)
				r.NotUpdated[id] = privacyConflictError()
			}
			return nil
		},
	}, req)
}

func (s *Service) taskQuery(c *jmap.Call, raw json.RawMessage) (any, error) {
	var extra struct {
		ExpandRecurrences bool    `json:"expandRecurrences"`
		TimeZone          *string `json:"timeZone"`
	}
	req, err := std.ParseQuery(raw, &extra)
	if err != nil {
		return nil, err
	}
	if extra.ExpandRecurrences {
		return nil, jmap.Errorf(ErrCannotCalculateOccurrences, "this server does not expand recurrences")
	}
	loc := time.UTC
	if extra.TimeZone != nil {
		var ok bool
		if loc, ok = loadZone(*extra.TimeZone); !ok {
			return nil, jmap.ArgError("timeZone must be an IANA time zone id")
		}
	}

	// The filter and sort are handed on as decoded JSON, which is what
	// the code that reads them was written for.
	var filter any
	if req.Filter != nil {
		if filter, err = jmap.DecodeValue(req.Filter); err != nil {
			return nil, jmap.ArgError("filter: %v", err)
		}
	}
	var sort any
	if req.Sort != nil {
		list := make([]any, len(req.Sort))
		for i, rawCmp := range req.Sort {
			if list[i], err = jmap.DecodeValue(rawCmp); err != nil {
				return nil, jmap.ArgError("sort: %v", err)
			}
		}
		sort = list
	}
	comparators, sortErr := parseSort(sort)
	if sortErr != nil {
		return nil, sortErr
	}

	return std.Query(c, s.beginTxn, limits, std.Querier[*txn]{
		Type: typeTask,
		All: func(_ context.Context, tx *txn, _ *std.QueryRequest) ([]string, error) {
			match, filterErr := compileFilter(c, tx.Tx, filter, loc)
			if filterErr != nil {
				return nil, filterErr
			}
			rows, err := loadQueryRows(tx.Tx, loc)
			if err != nil {
				return nil, err
			}
			var matched []*queryRow
			for _, r := range rows {
				if match(r) {
					matched = append(matched, r)
				}
			}
			slices.SortStableFunc(matched, func(a, b *queryRow) int {
				for _, cmp := range comparators {
					if n := cmp.compare(a, b); n != 0 {
						return n
					}
				}
				return strings.Compare(a.id, b.id)
			})
			ids := make([]string, 0, len(matched))
			for _, r := range matched {
				ids = append(ids, r.id)
			}
			return ids, nil
		},
	}, req)
}

// ---- /changes -----------------------------------------------------------

// changes answers a /changes over a table that has the created_modseq,
// updated_modseq and destroyed_modseq columns.
func (s *Service) changes(c *jmap.Call, raw json.RawMessage, table, typ string) (any, error) {
	req, err := std.ParseChanges(raw, nil)
	if err != nil {
		return nil, err
	}
	return std.Changes(c, s.beginTxn, req, func(_ context.Context, tx *txn, sinceState string, maxChanges int) (*std.Changed, error) {
		since, ok := store.ParseState(sinceState)
		if !ok {
			return nil, &jmap.MethodError{Type: jmap.ErrCannotCalculateChanges}
		}
		current, err := tx.Tx.State(typ)
		if err != nil {
			return nil, err
		}
		if since > current {
			return nil, &jmap.MethodError{Type: jmap.ErrCannotCalculateChanges}
		}

		rows, err := tx.Query(fmt.Sprintf(`
			SELECT id, created_modseq, updated_modseq, destroyed_modseq
			FROM %s WHERE updated_modseq > ? ORDER BY updated_modseq`, table), since)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		ch := &std.Changed{}
		newState := current
		count := 0
		for rows.Next() {
			var id string
			var cm, um int64
			var dm sql.NullInt64
			if err := rows.Scan(&id, &cm, &um, &dm); err != nil {
				return nil, err
			}
			if maxChanges > 0 && count >= maxChanges {
				ch.HasMore = true
				break
			}
			switch {
			case dm.Valid && cm > since:
				// Created and destroyed since the client last looked; it
				// never needs to hear about this object.
				continue
			case dm.Valid:
				ch.Destroyed = append(ch.Destroyed, id)
			case cm > since:
				ch.Created = append(ch.Created, id)
			default:
				ch.Updated = append(ch.Updated, id)
			}
			count++
			newState = um
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if !ch.HasMore {
			newState = current
		}
		ch.NewState = store.StateString(newState)
		return ch, nil
	})
}
