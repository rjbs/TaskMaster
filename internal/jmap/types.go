// Package jmap implements the protocol-level pieces of JMAP (RFC 8620) that
// are independent of any particular data type: request and response framing,
// JSON Pointer evaluation, PatchObject application, and method dispatch.
package jmap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
)

// Invocation is a method call or method response: a three-element array of
// method name, arguments object, and client-chosen call id.
type Invocation struct {
	Name   string
	Args   map[string]any
	CallID string
}

func (inv Invocation) MarshalJSON() ([]byte, error) {
	args := inv.Args
	if args == nil {
		args = map[string]any{}
	}
	return json.Marshal([]any{inv.Name, args, inv.CallID})
}

func (inv *Invocation) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw) != 3 {
		return fmt.Errorf("invocation must have exactly three elements, not %d", len(raw))
	}
	if err := json.Unmarshal(raw[0], &inv.Name); err != nil {
		return fmt.Errorf("invocation name: %w", err)
	}
	args, err := DecodeObject(raw[1])
	if err != nil {
		return fmt.Errorf("invocation arguments: %w", err)
	}
	inv.Args = args
	if err := json.Unmarshal(raw[2], &inv.CallID); err != nil {
		return fmt.Errorf("invocation call id: %w", err)
	}
	return nil
}

// DecodeValue decodes arbitrary JSON into Go values, keeping numbers as
// json.Number so that values we store and later re-encode round-trip exactly.
func DecodeValue(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return v, nil
}

// DecodeObject is DecodeValue restricted to a JSON object.
func DecodeObject(data []byte) (map[string]any, error) {
	v, err := DecodeValue(data)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected a JSON object")
	}
	return m, nil
}

// Request is the JMAP Request object (RFC 8620 §3.3).
type Request struct {
	Using       []string          `json:"using"`
	MethodCalls []Invocation      `json:"methodCalls"`
	CreatedIds  map[string]string `json:"createdIds,omitempty"`
}

// Response is the JMAP Response object (RFC 8620 §3.4).
type Response struct {
	MethodResponses []Invocation      `json:"methodResponses"`
	CreatedIds      map[string]string `json:"createdIds,omitempty"`
	SessionState    string            `json:"sessionState"`
}

// MethodError is a method-level error (RFC 8620 §3.6.2).  Handlers return
// one of these to have the whole method call replaced by an "error"
// response.
type MethodError struct {
	Type        string
	Description string
}

func (e *MethodError) Error() string {
	if e.Description == "" {
		return e.Type
	}
	return e.Type + ": " + e.Description
}

// Errorf builds a MethodError with a formatted description.
func Errorf(typ, format string, args ...any) *MethodError {
	return &MethodError{Type: typ, Description: fmt.Sprintf(format, args...)}
}

// Standard method-level error types from RFC 8620 §3.6.2 and §5.
const (
	ErrServerFail             = "serverFail"
	ErrUnknownMethod          = "unknownMethod"
	ErrInvalidArguments       = "invalidArguments"
	ErrInvalidResultReference = "invalidResultReference"
	ErrForbidden              = "forbidden"
	ErrAccountNotFound        = "accountNotFound"
	ErrAccountNotSupported    = "accountNotSupportedByMethod"
	ErrAccountReadOnly        = "accountReadOnly"
	ErrRequestTooLarge        = "requestTooLarge"
	ErrStateMismatch          = "stateMismatch"
	ErrCannotCalculateChanges = "cannotCalculateChanges"
	ErrUnsupportedFilter      = "unsupportedFilter"
	ErrUnsupportedSort        = "unsupportedSort"
	ErrAnchorNotFound         = "anchorNotFound"
)

// SetError is the per-object error in a /set response (RFC 8620 §5.3).
type SetError struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Properties  []string `json:"properties,omitempty"`
	ExistingID  string   `json:"existingId,omitempty"`
}

func (e *SetError) Error() string {
	return e.Type + ": " + e.Description
}

// Standard SetError types from RFC 8620 §5.3.
const (
	SetErrForbidden         = "forbidden"
	SetErrOverQuota         = "overQuota"
	SetErrTooLarge          = "tooLarge"
	SetErrRateLimit         = "rateLimit"
	SetErrNotFound          = "notFound"
	SetErrInvalidPatch      = "invalidPatch"
	SetErrWillDestroy       = "willDestroy"
	SetErrInvalidProperties = "invalidProperties"
	SetErrSingleton         = "singleton"
	SetErrAlreadyExists     = "alreadyExists"
)

// InvalidProperties builds an invalidProperties SetError naming the
// offending properties.
func InvalidProperties(desc string, props ...string) *SetError {
	return &SetError{Type: SetErrInvalidProperties, Description: desc, Properties: props}
}

// AlreadyExists is the SetError for a create that duplicates an existing
// record.  RFC 8620 §5.4 defines it for /copy; it carries the id of the
// record that already exists.
func AlreadyExists(desc, existingID string) *SetError {
	return &SetError{Type: SetErrAlreadyExists, Description: desc, ExistingID: existingID}
}

// NotFound is the SetError for an unknown id.
func NotFound() *SetError {
	return &SetError{Type: SetErrNotFound}
}

// RequestError is a request-level error, returned as an RFC 7807 problem
// details object (RFC 8620 §3.6.1).
type RequestError struct {
	Type   string `json:"type"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	Limit  string `json:"limit,omitempty"`
}

func (e *RequestError) Error() string {
	return e.Type + ": " + e.Detail
}

const (
	ReqErrUnknownCapability = "urn:ietf:params:jmap:error:unknownCapability"
	ReqErrNotJSON           = "urn:ietf:params:jmap:error:notJSON"
	ReqErrNotRequest        = "urn:ietf:params:jmap:error:notRequest"
	ReqErrLimit             = "urn:ietf:params:jmap:error:limit"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)

// ValidID reports whether s is a syntactically valid JMAP Id (RFC 8620 §1.2).
func ValidID(s string) bool {
	return idPattern.MatchString(s)
}
