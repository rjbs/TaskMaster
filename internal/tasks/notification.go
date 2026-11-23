package tasks

import "github.com/rjbs/gizmo/jmap"

// TaskNotifications record changes made by other users.  There are no other
// users, so the collection is permanently empty and its state never moves.
// These methods exist so a conforming client that asks about notifications
// gets well-formed answers instead of unknownMethod.

const notificationState = "0"

func (s *Service) notificationGet(c *jmap.Call, args map[string]any) (map[string]any, error) {
	ids, _, err := argStringList(args, "ids")
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []string{}
	}
	return map[string]any{
		"accountId": s.cfg.AccountID,
		"state":     notificationState,
		"list":      []any{},
		"notFound":  ids,
	}, nil
}

func (s *Service) notificationChanges(c *jmap.Call, args map[string]any) (map[string]any, error) {
	since, ok, err := argString(args, "sinceState")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, jmap.ArgError("sinceState is required")
	}
	return map[string]any{
		"accountId":      s.cfg.AccountID,
		"oldState":       since,
		"newState":       notificationState,
		"hasMoreChanges": false,
		"created":        []string{},
		"updated":        []string{},
		"destroyed":      []string{},
	}, nil
}

func (s *Service) notificationSet(c *jmap.Call, args map[string]any) (map[string]any, error) {
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

	notCreated := map[string]any{}
	for cid := range create {
		notCreated[cid] = &jmap.SetError{Type: jmap.SetErrForbidden, Description: "notifications are created only by the server"}
	}
	notUpdated := map[string]any{}
	for id := range update {
		notUpdated[id] = &jmap.SetError{Type: jmap.SetErrForbidden, Description: "notifications cannot be updated"}
	}
	notDestroyed := map[string]any{}
	for _, id := range destroy {
		notDestroyed[id] = jmap.NotFound()
	}

	return map[string]any{
		"accountId":    s.cfg.AccountID,
		"oldState":     notificationState,
		"newState":     notificationState,
		"created":      nil,
		"notCreated":   nilIfEmpty(notCreated),
		"updated":      nil,
		"notUpdated":   nilIfEmpty(notUpdated),
		"destroyed":    []string{},
		"notDestroyed": nilIfEmpty(notDestroyed),
	}, nil
}

func (s *Service) notificationQuery(c *jmap.Call, args map[string]any) (map[string]any, error) {
	resp := map[string]any{
		"accountId":           s.cfg.AccountID,
		"queryState":          notificationState,
		"canCalculateChanges": true,
		"position":            0,
		"ids":                 []string{},
	}
	if total, err := argBool(args, "calculateTotal", false); err != nil {
		return nil, err
	} else if total {
		resp["total"] = 0
	}
	return resp, nil
}

func (s *Service) notificationQueryChanges(c *jmap.Call, args map[string]any) (map[string]any, error) {
	since, ok, err := argString(args, "sinceQueryState")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, jmap.ArgError("sinceQueryState is required")
	}
	return map[string]any{
		"accountId":     s.cfg.AccountID,
		"oldQueryState": since,
		"newQueryState": notificationState,
		"removed":       []string{},
		"added":         []any{},
	}, nil
}
