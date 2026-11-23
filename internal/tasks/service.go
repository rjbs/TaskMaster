// Package tasks implements the JMAP for Task Lists data types (TaskList,
// Task, and a deliberately empty TaskNotification) on top of the store.
//
// Each account has its own database and its own Service; a Registry routes
// method calls to the Service for the accountId they name.  Every account
// has exactly one user, its owner, so everything the specification says
// about sharing, per-user properties, rights and notifications collapses:
// the owner has every right and is never notified of changes made by
// anyone else.
package tasks

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rjbs/gizmo/auth"
	"github.com/rjbs/gizmo/jmap"
	"github.com/rjbs/taskmaster/internal/store"
)

// Capability is the JMAP capability URI this package implements.
const Capability = "urn:ietf:params:jmap:tasks"

const (
	typeTaskList = store.TypeTaskList
	typeTask     = store.TypeTask
)

// Config holds the deployment-specific settings of the service.
type Config struct {
	// AccountID is the account this service serves.
	AccountID string

	// DefaultTimeZone resolves floating date-times (tasks with no
	// "timeZone") into instants for filtering and sorting.  The spec
	// falls back to the Principal's zone, which we do not have.
	DefaultTimeZone *time.Location
}

type Service struct {
	db  *store.DB
	cfg Config
}

func New(db *store.DB, cfg Config) *Service {
	if cfg.DefaultTimeZone == nil {
		cfg.DefaultTimeZone = time.UTC
	}
	return &Service{db: db, cfg: cfg}
}

// AccountID is the id of the account this service serves.
func (s *Service) AccountID() string { return s.cfg.AccountID }

// handler implements one JMAP method for one account.  Its arguments are
// decoded JSON, with numbers as json.Number.
type handler func(c *jmap.Call, args map[string]any) (map[string]any, error)

// standardMethods maps each JMAP method that Gizmo's drivers answer to the
// Service method that sets them to work; see std.go.
var standardMethods = map[string]func(*Service, *jmap.Call, json.RawMessage) (any, error){
	"TaskList/get":     (*Service).taskListGet,
	"TaskList/changes": (*Service).taskListChanges,
	"TaskList/set":     (*Service).taskListSet,
	"Task/get":         (*Service).taskGet,
	"Task/changes":     (*Service).taskChanges,
	"Task/set":         (*Service).taskSet,
	"Task/query":       (*Service).taskQuery,
}

// methods maps each other JMAP method to the Service method implementing
// it.
var methods = map[string]func(*Service) handler{
	"Task/queryChanges":             func(s *Service) handler { return s.taskQueryChanges },
	"TaskNotification/get":          func(s *Service) handler { return s.notificationGet },
	"TaskNotification/changes":      func(s *Service) handler { return s.notificationChanges },
	"TaskNotification/set":          func(s *Service) handler { return s.notificationSet },
	"TaskNotification/query":        func(s *Service) handler { return s.notificationQuery },
	"TaskNotification/queryChanges": func(s *Service) handler { return s.notificationQueryChanges },
}

// Registry holds the Service for every account the server has.
type Registry struct {
	services map[string]*Service
}

func NewRegistry() *Registry {
	return &Registry{services: map[string]*Service{}}
}

// Add makes svc the service for its account.
func (r *Registry) Add(svc *Service) {
	r.services[svc.AccountID()] = svc
}

// Get returns the service for an account, or nil.
func (r *Registry) Get(accountID string) *Service {
	return r.services[accountID]
}

// Register adds every method to p.  Each call is routed to the Service for
// its accountId, provided the request's authenticated user owns that
// account; otherwise the call fails with accountNotFound, which is what RFC
// 8620 prescribes for an account the user cannot see.
func (r *Registry) Register(p *jmap.Processor) {
	find := func(id string) (*Service, bool) {
		svc, ok := r.services[id]
		return svc, ok
	}
	for name, method := range standardMethods {
		p.Register(name, Capability, auth.ForAccount(find, method))
	}
	for name, pick := range methods {
		p.Register(name, Capability, auth.ForAccount(find, func(svc *Service, c *jmap.Call, raw json.RawMessage) (any, error) {
			args, err := jmap.DecodeObject(raw)
			if err != nil {
				return nil, jmap.ArgError("%v", err)
			}
			return pick(svc)(c, args)
		}))
	}
}

// Capabilities is the account's accountCapabilities object.
func (s *Service) Capabilities() map[string]any {
	return map[string]any{Capability: s.AccountCapabilities()}
}

// AccountCapabilities is the value of the tasks capability in the account's
// accountCapabilities object.
func (s *Service) AccountCapabilities() map[string]any {
	return map[string]any{
		"maxTaskListsPerTask":      nil,
		"minDateTime":              "1900-01-01T00:00:00Z",
		"maxDateTime":              "2200-01-01T00:00:00Z",
		"maxExpandedQueryDuration": "P1Y",
		"mayCreateTaskList":        true,
	}
}

// EnsureDefaultTaskList creates a default list named "Tasks" if the account
// has no task lists at all, so clients always have somewhere to put things.
func (s *Service) EnsureDefaultTaskList(ctx context.Context) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM tasklists`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	l := &taskList{ID: newID("L-"), Name: "Tasks", IsSubscribed: true, IsVisible: true, IsDefault: true}
	if err := insertTaskList(tx, l); err != nil {
		return err
	}
	return tx.Commit()
}

func newID(prefix string) string {
	return prefix + uuid.NewString()
}

func now() string {
	return formatUTC(time.Now())
}

// encodeJSON marshals without HTML escaping, so stored data stays legible.
func encodeJSON(v any) (string, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(sb.String(), "\n"), nil
}

// Argument accessors.  Each treats a JSON null the same as an absent key.

func argString(args map[string]any, key string) (string, bool, *jmap.MethodError) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", false, jmap.ArgError("%s must be a string", key)
	}
	return s, true, nil
}

func argBool(args map[string]any, key string, def bool) (bool, *jmap.MethodError) {
	v, ok := args[key]
	if !ok || v == nil {
		return def, nil
	}
	b, ok := v.(bool)
	if !ok {
		return def, jmap.ArgError("%s must be a boolean", key)
	}
	return b, nil
}

func argStringList(args map[string]any, key string) ([]string, bool, *jmap.MethodError) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, false, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, false, jmap.ArgError("%s must be an array of strings", key)
	}
	out := make([]string, 0, len(list))
	for _, el := range list {
		s, ok := el.(string)
		if !ok {
			return nil, false, jmap.ArgError("%s must be an array of strings", key)
		}
		out = append(out, s)
	}
	return out, true, nil
}

func argObject(args map[string]any, key string) (map[string]any, bool, *jmap.MethodError) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, false, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false, jmap.ArgError("%s must be an object", key)
	}
	return m, true, nil
}

// intValue extracts an integer from a decoded JSON value.
func intValue(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}

// nilIfEmpty turns an empty map into nil so it encodes as JSON null, which
// is how /set responses report "none" for their result maps.
func nilIfEmpty[M ~map[string]V, V any](m M) M {
	if len(m) == 0 {
		return nil
	}
	return m
}

// States returns the current state string of every data type this service
// serves, keyed by type name.
func (s *Service) States(ctx context.Context) (map[string]string, error) {
	states, err := s.db.States(ctx)
	if err != nil {
		return nil, err
	}
	states["TaskNotification"] = notificationState
	return states, nil
}
