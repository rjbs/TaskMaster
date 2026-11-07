package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rjbs/taskmaster/internal/auth"
	"github.com/rjbs/taskmaster/internal/jmap"
	"github.com/rjbs/taskmaster/internal/store"
	"github.com/rjbs/taskmaster/internal/tasks"
)

// multiEnv is a server with several users and accounts, reachable through
// whichever middleware a test wraps around it.
type multiEnv struct {
	t      *testing.T
	dir    *auth.Directory
	srv    *Server
	broker *Broker
}

func newMultiEnv(t *testing.T, users map[string]*auth.User, accounts map[string]*auth.Account) *multiEnv {
	t.Helper()
	dir, err := auth.NewDirectory(users, accounts)
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("America/New_York")
	reg := tasks.NewRegistry()
	broker := NewBroker()
	for id := range accounts {
		db, err := store.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		svc := tasks.New(db, tasks.Config{AccountID: id, DefaultTimeZone: loc})
		if err := svc.EnsureDefaultTaskList(t.Context()); err != nil {
			t.Fatal(err)
		}
		db.OnCommit(broker.Publisher(id))
		reg.Add(svc)
	}
	proc := jmap.NewProcessor()
	reg.Register(proc)
	srv := New(Config{Directory: dir, Accounts: reg, Processor: proc, Broker: broker})
	return &multiEnv{t: t, dir: dir, srv: srv, broker: broker}
}

// as serves requests as the given user, standing in for a listener's
// middleware.
func (m *multiEnv) as(u *auth.User) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.srv.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), u)))
	}))
	m.t.Cleanup(ts.Close)
	return ts
}

func getSession(t *testing.T, url string) obj {
	t.Helper()
	resp, err := http.Get(url + "/.well-known/jmap")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sess obj
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		t.Fatal(err)
	}
	return sess
}

func sessionAccounts(sess obj) []string {
	var ids []string
	for id := range sess["accounts"].(obj) {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func callAs(t *testing.T, url, method string, args obj) jmap.Invocation {
	t.Helper()
	e := &env{t: t, ts: &httptest.Server{URL: url}}
	return e.request(jmap.Invocation{Name: method, Args: args, CallID: "c"})[0]
}

var twoUsers = map[string]*auth.User{
	"rjbs":  {Credentials: []auth.Credential{{Type: auth.CredLoopback}}, PrimaryAccount: "rjbs"},
	"guest": {Credentials: []auth.Credential{{Type: auth.CredTailscale, Login: "guest@example"}}},
}

var threeAccounts = map[string]*auth.Account{
	"rjbs":  {Name: "Personal", Owner: "rjbs"},
	"work":  {Name: "Work", Owner: "rjbs"},
	"guest": {Name: "Guest", Owner: "guest"},
}

// checkSession fetches the Session as user u and checks its username,
// its primary tasks account (empty meaning there must be none), and the
// accounts it lists.
func checkSession(t *testing.T, m *multiEnv, u *auth.User, username, primary string, accounts []string) {
	t.Helper()
	sess := getSession(t, m.as(u).URL)
	if sess["username"] != username {
		t.Errorf("%s: username = %v", username, sess["username"])
	}
	got, has := sess["primaryAccounts"].(obj)[tasks.Capability]
	if primary == "" && has {
		t.Errorf("%s: primary account = %v, want none", username, got)
	} else if primary != "" && got != primary {
		t.Errorf("%s: primary account = %v, want %s", username, got, primary)
	}
	if got := sessionAccounts(sess); !reflect.DeepEqual(got, accounts) {
		t.Errorf("%s: accounts = %v, want %v", username, got, accounts)
	}
}

func TestSessionListsOnlyOwnedAccounts(t *testing.T) {
	m := newMultiEnv(t, twoUsers, threeAccounts)
	checkSession(t, m, m.dir.Users["rjbs"], "rjbs", "rjbs", []string{"rjbs", "work"})
	checkSession(t, m, m.dir.Users["guest"], "guest", "guest", []string{"guest"})

	rjbs := getSession(t, m.as(m.dir.Users["rjbs"]).URL)
	if got := rjbs["accounts"].(obj)["work"].(obj)["name"]; got != "Work" {
		t.Errorf("account name = %v", got)
	}
	guest := getSession(t, m.as(m.dir.Users["guest"]).URL)
	if rjbs["state"] == guest["state"] {
		t.Error("two users with different accounts share a session state")
	}
}

func TestSessionWithoutPrimaryAccount(t *testing.T) {
	m := newMultiEnv(t,
		map[string]*auth.User{"rjbs": {Credentials: []auth.Credential{{Type: auth.CredLoopback}}}},
		map[string]*auth.Account{"rjbs": {Owner: "rjbs"}, "work": {Owner: "rjbs"}},
	)
	checkSession(t, m, m.dir.Users["rjbs"], "rjbs", "", []string{"rjbs", "work"})
}

func TestCredentialLimitedToAccount(t *testing.T) {
	m := newMultiEnv(t,
		map[string]*auth.User{"rjbs": {PrimaryAccount: "rjbs", Credentials: []auth.Credential{
			{Type: auth.CredLoopback},
			{Type: auth.CredTailscale, Login: "home@example", AccountIDs: []string{"side", "rjbs"}},
			{Type: auth.CredTailscale, Login: "work@example", AccountIDs: []string{"work", "side"}},
		}}},
		map[string]*auth.Account{"rjbs": {Owner: "rjbs"}, "work": {Owner: "rjbs"}, "side": {Owner: "rjbs"}},
	)
	checkSession(t, m, m.dir.LoopbackUser(), "rjbs", "rjbs", []string{"rjbs", "side", "work"})
	checkSession(t, m, m.dir.TailscaleUser("home@example"), "rjbs", "rjbs", []string{"rjbs", "side"})
	checkSession(t, m, m.dir.TailscaleUser("work@example"), "rjbs", "", []string{"side", "work"})

	work := m.as(m.dir.TailscaleUser("work@example")).URL
	for _, acct := range []string{"work", "side"} {
		if inv := callAs(t, work, "TaskList/get", obj{"accountId": acct}); inv.Name != "TaskList/get" {
			t.Errorf("limited credential, permitted account %s: %v", acct, inv.Args)
		}
	}
	inv := callAs(t, work, "TaskList/get", obj{"accountId": "rjbs"})
	if inv.Name != "error" || inv.Args["type"] != jmap.ErrAccountNotFound {
		t.Errorf("limited credential reached rjbs: %v", inv)
	}
}

func TestMethodsRouteByAccount(t *testing.T) {
	m := newMultiEnv(t, twoUsers, threeAccounts)
	rjbs := m.as(m.dir.Users["rjbs"]).URL
	guest := m.as(m.dir.Users["guest"]).URL

	checkAccess := func(url, account, want string) {
		t.Helper()
		inv := callAs(t, url, "TaskList/get", obj{"accountId": account, "ids": nil})
		got := inv.Name
		if got == "error" {
			got = inv.Args["type"].(string)
		}
		if got != want {
			t.Errorf("%s via %s: got %s, want %s", account, url, got, want)
		}
	}
	checkAccess(rjbs, "rjbs", "TaskList/get")
	checkAccess(rjbs, "work", "TaskList/get")
	checkAccess(rjbs, "guest", jmap.ErrAccountNotFound)
	checkAccess(rjbs, "nobody", jmap.ErrAccountNotFound)
	checkAccess(guest, "guest", "TaskList/get")
	checkAccess(guest, "rjbs", jmap.ErrAccountNotFound)

	// Data written to one account is invisible from another, even for the
	// same user.
	inv := callAs(t, rjbs, "TaskList/set", obj{"accountId": "work", "create": obj{"k": obj{"name": "Projects"}}})
	if inv.Name == "error" {
		t.Fatalf("create in work: %v", inv.Args)
	}
	count := func(account string) int {
		return len(callAs(t, rjbs, "TaskList/get", obj{"accountId": account, "ids": nil}).Args["list"].([]any))
	}
	if n := count("work"); n != 2 {
		t.Errorf("work has %d lists, want 2", n)
	}
	if n := count("rjbs"); n != 1 {
		t.Errorf("rjbs has %d lists, want 1", n)
	}
}

func TestEventSourceCoversOwnedAccounts(t *testing.T) {
	m := newMultiEnv(t, twoUsers, threeAccounts)
	rjbs := m.as(m.dir.Users["rjbs"]).URL

	resp, err := http.Get(rjbs + "/jmap/eventsource?types=*&closeafter=no&ping=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, data := readEvent(t, resp.Body)
	changed := data["changed"].(obj)
	if _, ok := changed["work"]; !ok || len(changed) != 2 {
		t.Errorf("initial event covers %v, want rjbs and work", changed)
	}

	callAs(t, rjbs, "TaskList/set", obj{"accountId": "work", "create": obj{"k": obj{"name": "Projects"}}})
	_, data = readEvent(t, resp.Body)
	changed = data["changed"].(obj)
	if _, ok := changed["work"]; !ok || len(changed) != 1 {
		t.Errorf("change event covers %v, want only work", changed)
	}

	// Another user's account changing is none of this stream's business.
	callAs(t, m.as(m.dir.Users["guest"]).URL, "TaskList/set", obj{"accountId": "guest", "create": obj{"k": obj{"name": "Secret"}}})
	callAs(t, rjbs, "TaskList/set", obj{"accountId": "rjbs", "create": obj{"k": obj{"name": "Mine"}}})
	_, data = readEvent(t, resp.Body)
	changed = data["changed"].(obj)
	if _, ok := changed["rjbs"]; !ok || len(changed) != 1 {
		t.Errorf("after guest and rjbs changes, event covers %v, want only rjbs", changed)
	}
}

func TestUnauthenticatedRequestsAreRefused(t *testing.T) {
	m := newMultiEnv(t, twoUsers, threeAccounts)
	bare := httptest.NewServer(m.srv)
	t.Cleanup(bare.Close)
	if resp, _ := http.Get(bare.URL + "/.well-known/jmap"); resp.StatusCode != 401 {
		t.Errorf("no user in context: status %d", resp.StatusCode)
	}

	noLoopback, _ := auth.NewDirectory(
		map[string]*auth.User{"x": {}},
		map[string]*auth.Account{"x": {Owner: "x"}},
	)
	ts := httptest.NewServer(auth.Loopback(noLoopback, m.srv))
	t.Cleanup(ts.Close)
	checkAuthorized(t, ts.URL, "", 401, "", nil)
}

func TestTailscaleMiddleware(t *testing.T) {
	m := newMultiEnv(t, twoUsers, threeAccounts)

	identities := map[string]auth.Identity{}
	var whoisErr error
	whois := func(ctx context.Context, remoteAddr string) (auth.Identity, error) {
		if whoisErr != nil {
			return auth.Identity{}, whoisErr
		}
		return identities["current"], nil
	}
	var rejected string
	reject := func(r *http.Request, why string) { rejected = why }
	ts := httptest.NewServer(auth.Tailscale(m.dir, whois, reject, m.srv))
	t.Cleanup(ts.Close)

	check := func(name string, id auth.Identity, err error, wantStatus int, wantUser string) {
		t.Helper()
		identities["current"], whoisErr, rejected = id, err, ""
		resp, gerr := http.Get(ts.URL + "/.well-known/jmap")
		if gerr != nil {
			t.Fatal(gerr)
		}
		defer resp.Body.Close()
		if resp.StatusCode != wantStatus {
			t.Errorf("%s: status %d, want %d", name, resp.StatusCode, wantStatus)
		}
		if wantUser != "" {
			var sess obj
			json.NewDecoder(resp.Body).Decode(&sess)
			if sess["username"] != wantUser {
				t.Errorf("%s: username %v, want %s", name, sess["username"], wantUser)
			}
		} else if rejected == "" {
			t.Errorf("%s: refused without telling the operator why", name)
		}
	}
	check("known login", auth.Identity{Login: "guest@example"}, nil, 200, "guest")
	check("unknown login", auth.Identity{Login: "stranger@example"}, nil, 403, "")
	check("tagged node", auth.Identity{Login: "tagged-devices", Tagged: true}, nil, 403, "")
	check("whois failure", auth.Identity{}, errors.New("no such peer"), 401, "")
}

// checkAuthorized fetches the Session from url with the given
// Authorization header (none if empty) and checks the status and, for a
// success, the user and accounts the Session describes.
func checkAuthorized(t *testing.T, url, authz string, wantStatus int, wantUser string, wantAccounts []string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url+"/.well-known/jmap", nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Errorf("%q: status %d, want %d", authz, resp.StatusCode, wantStatus)
		return
	}
	if wantStatus != 200 {
		if resp.StatusCode == 401 && resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%q: 401 without WWW-Authenticate", authz)
		}
		return
	}
	var sess obj
	json.NewDecoder(resp.Body).Decode(&sess)
	if sess["username"] != wantUser {
		t.Errorf("%q: username %v, want %s", authz, sess["username"], wantUser)
	}
	if got := sessionAccounts(sess); !reflect.DeepEqual(got, wantAccounts) {
		t.Errorf("%q: accounts %v, want %v", authz, got, wantAccounts)
	}
}

func TestBearerMiddleware(t *testing.T) {
	m := newMultiEnv(t,
		map[string]*auth.User{
			"rjbs": {Credentials: []auth.Credential{
				{Type: auth.CredLoopback},
				{Type: auth.CredBearer, TokenHash: auth.HashToken("tm_work"), AccountIDs: []string{"work"}},
			}},
			"guest": {Credentials: []auth.Credential{
				{Type: auth.CredBearer, TokenHash: strings.ToUpper(auth.HashToken("tm_guest"))},
			}},
		},
		threeAccounts,
	)
	var rejected []string
	reject := func(r *http.Request, why string) { rejected = append(rejected, why) }

	plain := httptest.NewServer(auth.Bearer(m.dir, reject, auth.Loopback(m.dir, m.srv)))
	t.Cleanup(plain.Close)
	checkAuthorized(t, plain.URL, "", 200, "rjbs", []string{"rjbs", "work"})
	checkAuthorized(t, plain.URL, "Bearer tm_work", 200, "rjbs", []string{"work"})
	checkAuthorized(t, plain.URL, "Bearer tm_guest", 200, "guest", []string{"guest"})
	checkAuthorized(t, plain.URL, "bearer tm_guest", 200, "guest", []string{"guest"})
	checkAuthorized(t, plain.URL, "Basic dXNlcjpwYXNz", 200, "rjbs", []string{"rjbs", "work"})
	checkAuthorized(t, plain.URL, "Bearer tm_nonsense", 401, "", nil)
	if len(rejected) != 1 {
		t.Errorf("rejections reported: %v", rejected)
	}

	// On the tailnet, a token admits even a tagged node, which has no
	// login of its own.
	whois := func(ctx context.Context, remoteAddr string) (auth.Identity, error) {
		return auth.Identity{Login: "tagged-devices", Tagged: true}, nil
	}
	tailnet := httptest.NewServer(auth.Bearer(m.dir, reject, auth.Tailscale(m.dir, whois, reject, m.srv)))
	t.Cleanup(tailnet.Close)
	checkAuthorized(t, tailnet.URL, "Bearer tm_guest", 200, "guest", []string{"guest"})
	checkAuthorized(t, tailnet.URL, "", 403, "", nil)
}
