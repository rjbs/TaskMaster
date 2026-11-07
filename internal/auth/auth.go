// Package auth knows who is asking.  It holds the directory of users,
// accounts and credentials, and provides the HTTP middleware that turns a
// connection into an authenticated User in the request context.
//
// The model is small on purpose.  A user is an actor who can authenticate.
// An account is a JMAP account, a separate body of data with its own
// SQLite file, owned by exactly one user who has complete access to it; a
// user may own several.  A credential is one way a user proves who they
// are.  There is no sharing, no admin, and no notion of rights beyond
// ownership.
package auth

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
)

// Credential types.
const (
	// CredLoopback means "a request arriving on the plain, unauthenticated
	// listener".  At most one user may hold it.
	CredLoopback = "loopback"

	// CredTailscale identifies a user by the login name Tailscale reports
	// for the node the request came from.
	CredTailscale = "tailscale"
)

type Credential struct {
	Type string

	// Login is the Tailscale login name (like "rjbs@github") for a
	// CredTailscale credential, and empty otherwise.
	Login string
}

type User struct {
	ID          string
	Credentials []Credential

	// Accounts lists the ids of the accounts this user owns, sorted.
	Accounts []string

	// PrimaryAccount is the account the Session object advertises as
	// primary for every capability, or empty for none.  RFC 8620 §2 lets
	// a Session name no primary account.
	PrimaryAccount string
}

// HasAccount reports whether the user owns the account.
func (u *User) HasAccount(id string) bool {
	_, found := slices.BinarySearch(u.Accounts, id)
	return found
}

type Account struct {
	ID    string
	Name  string
	Owner string

	// Path is the account's SQLite file.
	Path string
}

// Directory is the complete set of users and accounts.  It is built once
// at startup and never changes while the server runs.
type Directory struct {
	Users    map[string]*User
	Accounts map[string]*Account

	loopback  *User
	tailscale map[string]*User
}

// NewDirectory links users to the accounts they own and indexes
// credentials.  It reports a bad directory rather than guessing: an
// account with an unknown owner, two users holding the loopback
// credential, or a primary account the user does not own.  A user who
// owns exactly one account and names no primary gets that one as primary;
// a user who owns several gets none unless one is named.
func NewDirectory(users map[string]*User, accounts map[string]*Account) (*Directory, error) {
	d := &Directory{
		Users:     users,
		Accounts:  accounts,
		tailscale: map[string]*User{},
	}
	for _, u := range users {
		u.Accounts = nil
	}
	for id, a := range accounts {
		a.ID = id
		owner, ok := users[a.Owner]
		if !ok {
			return nil, fmt.Errorf("account %q is owned by unknown user %q", id, a.Owner)
		}
		owner.Accounts = append(owner.Accounts, id)
	}
	for id, u := range users {
		u.ID = id
		sort.Strings(u.Accounts)
		switch {
		case len(u.Accounts) == 0:
			return nil, fmt.Errorf("user %q owns no accounts", id)
		case u.PrimaryAccount == "" && len(u.Accounts) == 1:
			u.PrimaryAccount = u.Accounts[0]
		case u.PrimaryAccount != "" && !u.HasAccount(u.PrimaryAccount):
			return nil, fmt.Errorf("user %q names primaryAccount %q, which it does not own", id, u.PrimaryAccount)
		}
		for _, c := range u.Credentials {
			switch c.Type {
			case CredLoopback:
				if d.loopback != nil && d.loopback != u {
					return nil, fmt.Errorf("users %q and %q both claim the loopback credential", d.loopback.ID, id)
				}
				d.loopback = u
			case CredTailscale:
				if c.Login == "" {
					return nil, fmt.Errorf("user %q has a tailscale credential with no login", id)
				}
				if other, dup := d.tailscale[c.Login]; dup && other != u {
					return nil, fmt.Errorf("users %q and %q both claim tailscale login %q", other.ID, id, c.Login)
				}
				d.tailscale[c.Login] = u
			default:
				return nil, fmt.Errorf("user %q has a credential of unknown type %q", id, c.Type)
			}
		}
	}
	return d, nil
}

// LoopbackUser is the user that plain-listener requests act as, or nil if
// no user holds the loopback credential.
func (d *Directory) LoopbackUser() *User { return d.loopback }

// TailscaleUser is the user holding the given Tailscale login, or nil.
func (d *Directory) TailscaleUser(login string) *User { return d.tailscale[login] }

type ctxKey struct{}

// WithUser returns a context carrying the authenticated user.
func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// UserFrom returns the authenticated user, if the request has one.
func UserFrom(ctx context.Context) (*User, bool) {
	u, ok := ctx.Value(ctxKey{}).(*User)
	return u, ok && u != nil
}

// Loopback is middleware for the plain listener: every request acts as the
// loopback user.  With no such user configured, every request is refused.
func Loopback(d *Directory, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := d.LoopbackUser()
		if u == nil {
			http.Error(w, "no user is configured for unauthenticated access", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
	})
}

// Identity is what a Tailscale lookup says about the far end of a
// connection.
type Identity struct {
	Login  string
	Tagged bool
}

// WhoIsFunc resolves a connection's remote address to its Tailscale
// identity.
type WhoIsFunc func(ctx context.Context, remoteAddr string) (Identity, error)

// Tailscale is middleware for the tailnet listener: the request acts as the
// user holding the connecting node's login as a credential.  Tagged nodes
// have no user and are refused, as is any login nobody holds.  Refusals are
// reported through reject, so an operator can see which login to add.
func Tailscale(d *Directory, whois WhoIsFunc, reject func(r *http.Request, why string), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := whois(r.Context(), r.RemoteAddr)
		if err != nil {
			reject(r, "whois failed: "+err.Error())
			http.Error(w, "could not identify the connecting node", http.StatusUnauthorized)
			return
		}
		if id.Tagged {
			reject(r, "tagged node")
			http.Error(w, "tagged nodes may not use this service", http.StatusForbidden)
			return
		}
		u := d.TailscaleUser(id.Login)
		if u == nil {
			reject(r, fmt.Sprintf("no user holds tailscale login %q", id.Login))
			http.Error(w, "no user is configured for this tailscale login", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
	})
}
