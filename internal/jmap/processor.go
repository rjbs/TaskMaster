package jmap

import (
	"context"
	"fmt"
	"strings"
)

const CoreCapability = "urn:ietf:params:jmap:core"

// Context carries per-request state to method handlers.
type Context struct {
	ctx        context.Context
	CreatedIDs map[string]string
	responses  []Invocation
}

// Context returns the request's context.Context.
func (c *Context) Context() context.Context {
	return c.ctx
}

// ResolveID maps a creation reference ("#cid") to the id it produced earlier
// in this request.  Other strings are returned unchanged.  The boolean is
// false only for a creation reference that cannot be resolved.
func (c *Context) ResolveID(id string) (string, bool) {
	if !strings.HasPrefix(id, "#") {
		return id, true
	}
	real, ok := c.CreatedIDs[id[1:]]
	return real, ok
}

// Handler implements one JMAP method.  Returning a *MethodError produces an
// "error" response; any other error becomes serverFail.
type Handler func(c *Context, args map[string]any) (map[string]any, error)

type method struct {
	capability string
	handler    Handler
}

// Processor dispatches the method calls in a Request.
type Processor struct {
	methods      map[string]method
	MaxCalls     int
	SessionState func() string
}

func NewProcessor() *Processor {
	return &Processor{
		methods:      map[string]method{},
		MaxCalls:     64,
		SessionState: func() string { return "0" },
	}
}

// Register adds a method, available only when the request's "using" list
// includes capability.
func (p *Processor) Register(name, capability string, h Handler) {
	p.methods[name] = method{capability: capability, handler: h}
}

// Process runs every method call in req in order and returns the Response,
// or a RequestError if the request as a whole is unacceptable.
func (p *Processor) Process(ctx context.Context, req *Request) (*Response, *RequestError) {
	using := map[string]bool{}
	for _, u := range req.Using {
		using[u] = true
	}
	if !using[CoreCapability] {
		return nil, &RequestError{
			Type:   ReqErrUnknownCapability,
			Status: 400,
			Detail: "the request must use " + CoreCapability,
		}
	}
	for u := range using {
		if !p.knowsCapability(u) {
			return nil, &RequestError{Type: ReqErrUnknownCapability, Status: 400, Detail: "unknown capability " + u}
		}
	}
	if len(req.MethodCalls) > p.MaxCalls {
		return nil, &RequestError{Type: ReqErrLimit, Status: 400, Limit: "maxCallsInRequest"}
	}

	c := &Context{ctx: ctx, CreatedIDs: map[string]string{}}
	for k, v := range req.CreatedIds {
		c.CreatedIDs[k] = v
	}

	for _, call := range req.MethodCalls {
		c.responses = append(c.responses, p.invoke(c, call, using))
	}

	resp := &Response{MethodResponses: c.responses, SessionState: p.SessionState()}
	if req.CreatedIds != nil || len(c.CreatedIDs) > 0 {
		resp.CreatedIds = c.CreatedIDs
	}
	return resp, nil
}

func (p *Processor) knowsCapability(u string) bool {
	if u == CoreCapability {
		return true
	}
	for _, m := range p.methods {
		if m.capability == u {
			return true
		}
	}
	return false
}

func (p *Processor) invoke(c *Context, call Invocation, using map[string]bool) Invocation {
	fail := func(err *MethodError) Invocation {
		args := map[string]any{"type": err.Type}
		if err.Description != "" {
			args["description"] = err.Description
		}
		return Invocation{Name: "error", Args: args, CallID: call.CallID}
	}

	m, ok := p.methods[call.Name]
	if !ok || !using[m.capability] {
		return fail(&MethodError{Type: ErrUnknownMethod})
	}

	args := map[string]any{}
	for k, v := range call.Args {
		args[k] = v
	}
	if err := c.resolveReferences(args); err != nil {
		return fail(err)
	}

	result, err := m.handler(c, args)
	if err != nil {
		if me, ok := err.(*MethodError); ok {
			return fail(me)
		}
		return fail(&MethodError{Type: ErrServerFail, Description: err.Error()})
	}
	return Invocation{Name: call.Name, Args: result, CallID: call.CallID}
}

// resolveReferences replaces "#arg" result references (RFC 8620 §3.7) with
// the values they point at in earlier responses.
func (c *Context) resolveReferences(args map[string]any) *MethodError {
	for k, v := range args {
		if !strings.HasPrefix(k, "#") {
			continue
		}
		name := k[1:]
		if _, dup := args[name]; dup {
			return Errorf(ErrInvalidArguments, "both %q and %q were given", k, name)
		}
		ref, ok := v.(map[string]any)
		if !ok {
			return Errorf(ErrInvalidResultReference, "%s is not a ResultReference object", k)
		}
		resultOf, _ := ref["resultOf"].(string)
		refName, _ := ref["name"].(string)
		path, _ := ref["path"].(string)

		var source *Invocation
		for i := range c.responses {
			r := &c.responses[i]
			if r.CallID == resultOf && r.Name == refName {
				source = r
				break
			}
		}
		if source == nil {
			return Errorf(ErrInvalidResultReference, "no prior %s response with call id %q", refName, resultOf)
		}
		val, err := ResolvePointer(source.Args, path)
		if err != nil {
			return Errorf(ErrInvalidResultReference, "%s: %v", k, err)
		}
		args[name] = val
		delete(args, k)
	}
	return nil
}

// ArgError is a convenience for the common invalidArguments case.
func ArgError(format string, a ...any) *MethodError {
	return &MethodError{Type: ErrInvalidArguments, Description: fmt.Sprintf(format, a...)}
}
