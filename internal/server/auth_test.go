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

	"github.com/rjbs/gizmo/auth"
	"github.com/rjbs/gizmo/jmap"
	"github.com/rjbs/gizmo/jmaptest"
	"github.com/rjbs/gizmo/push"
	"github.com/rjbs/taskmaster/internal/store"
	"github.com/rjbs/taskmaster/internal/tasks"
)

// multiEnv is a server with several users and accounts, reachable through
// whichever middleware a test wraps around it.
type multiEnv struct {
	t      *testing.T
	dir    *auth.Directory
	srv    *Server
	broker *push.Broker
}

func newMultiEnv(t *testing.T, users map[string]*auth.User, accounts map[string]*auth.Account) *multiEnv {
	t.Helper()
	dir, err := auth.NewDirectory(users, accounts)
	if err != nil {
		t.Fatal(err)
	}
	dir.TokenPrefix = "taskmaster-"
	loc, _ := time.LoadLocation("America/New_York")
	reg := tasks.NewRegistry()
	broker := push.NewBroker()
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

// as is a client making requests as the given user, through a server that
// stands in for a listener's middleware.
func (m *multiEnv) as(u *auth.User) *jmaptest.Tester {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.srv.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), u)))
	}))
	m.t.Cleanup(ts.Close)
	return jmaptest.New(m.t, ts.URL, jmap.CoreCapability, tasks.Capability)
}

func sessionAccounts(sess obj) []string {
	var ids []string
	for id := range sess["accounts"].(obj) {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// callAs makes one method call as c's user and returns the one sentence
// answering it, whether a response or an error.
func callAs(t *testing.T, c *jmaptest.Tester, method string, args obj) *jmaptest.Sentence {
	t.Helper()
	return c.Request(jmaptest.Do(method, args)).AssertSentences(1)[0]
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
	sess := m.as(u).Session()
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

	rjbs := m.as(m.dir.Users["rjbs"]).Session()
	if got := rjbs["accounts"].(obj)["work"].(obj)["name"]; got != "Work" {
		t.Errorf("account name = %v", got)
	}
	guest := m.as(m.dir.Users["guest"]).Session()
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

	work := m.as(m.dir.TailscaleUser("work@example"))
	for _, acct := range []string{"work", "side"} {
		callAs(t, work, "TaskList/get", obj{"accountId": acct}).AssertNamed("TaskList/get")
	}
	callAs(t, work, "TaskList/get", obj{"accountId": "rjbs"}).AssertError(jmap.ErrAccountNotFound)
}

func TestMethodsRouteByAccount(t *testing.T) {
	m := newMultiEnv(t, twoUsers, threeAccounts)
	rjbs := m.as(m.dir.Users["rjbs"])
	guest := m.as(m.dir.Users["guest"])

	checkAccess := func(who string, c *jmaptest.Tester, account, want string) {
		t.Helper()
		s := callAs(t, c, "TaskList/get", obj{"accountId": account, "ids": nil})
		got := s.Name
		if got == "error" {
			got = s.Str("type")
		}
		if got != want {
			t.Errorf("%s via %s: got %s, want %s", account, who, got, want)
		}
	}
	checkAccess("rjbs", rjbs, "rjbs", "TaskList/get")
	checkAccess("rjbs", rjbs, "work", "TaskList/get")
	checkAccess("rjbs", rjbs, "guest", jmap.ErrAccountNotFound)
	checkAccess("rjbs", rjbs, "nobody", jmap.ErrAccountNotFound)
	checkAccess("guest", guest, "guest", "TaskList/get")
	checkAccess("guest", guest, "rjbs", jmap.ErrAccountNotFound)

	// Data written to one account is invisible from another, even for the
	// same user.
	callAs(t, rjbs, "TaskList/set", obj{"accountId": "work", "create": obj{"k": obj{"name": "Projects"}}}).
		AsSet().AssertNoErrors()
	count := func(account string) int {
		return len(callAs(t, rjbs, "TaskList/get", obj{"accountId": account, "ids": nil}).
			AssertNamed("TaskList/get").Args["list"].([]any))
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
	rjbs := m.as(m.dir.Users["rjbs"])

	stream := rjbs.Listen("types=*&closeafter=no&ping=0")
	changed := stream.NextState()
	if _, ok := changed["work"]; !ok || len(changed) != 2 {
		t.Errorf("initial event covers %v, want rjbs and work", changed)
	}

	callAs(t, rjbs, "TaskList/set", obj{"accountId": "work", "create": obj{"k": obj{"name": "Projects"}}})
	changed = stream.NextState()
	if _, ok := changed["work"]; !ok || len(changed) != 1 {
		t.Errorf("change event covers %v, want only work", changed)
	}

	// Another user's account changing is none of this stream's business.
	callAs(t, m.as(m.dir.Users["guest"]), "TaskList/set", obj{"accountId": "guest", "create": obj{"k": obj{"name": "Secret"}}})
	callAs(t, rjbs, "TaskList/set", obj{"accountId": "rjbs", "create": obj{"k": obj{"name": "Mine"}}})
	changed = stream.NextState()
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
	workToken, _ := auth.NewToken("taskmaster-")
	guestToken, _ := auth.NewToken("taskmaster-")
	unknownToken, _ := auth.NewToken("taskmaster-")
	m := newMultiEnv(t,
		map[string]*auth.User{
			"rjbs": {Credentials: []auth.Credential{
				{Type: auth.CredLoopback},
				{Type: auth.CredBearer, TokenHash: auth.HashToken(workToken), AccountIDs: []string{"work"}},
			}},
			"guest": {Credentials: []auth.Credential{
				{Type: auth.CredBearer, TokenHash: strings.ToUpper(auth.HashToken(guestToken))},
			}},
		},
		threeAccounts,
	)
	var rejected []string
	reject := func(r *http.Request, why string) { rejected = append(rejected, why) }

	plain := httptest.NewServer(auth.Bearer(m.dir, reject, auth.Loopback(m.dir, m.srv)))
	t.Cleanup(plain.Close)
	checkAuthorized(t, plain.URL, "", 200, "rjbs", []string{"rjbs", "work"})
	checkAuthorized(t, plain.URL, "Bearer "+workToken, 200, "rjbs", []string{"work"})
	checkAuthorized(t, plain.URL, "Bearer "+guestToken, 200, "guest", []string{"guest"})
	checkAuthorized(t, plain.URL, "bearer "+guestToken, 200, "guest", []string{"guest"})
	checkAuthorized(t, plain.URL, "Basic dXNlcjpwYXNz", 200, "rjbs", []string{"rjbs", "work"})
	checkAuthorized(t, plain.URL, "Bearer "+unknownToken, 401, "", nil)
	// Change the last character, which spoils the checksum.  It must
	// differ from the original, or the token is still the guest's.
	last := "x"
	if strings.HasSuffix(guestToken, last) {
		last = "y"
	}
	checkAuthorized(t, plain.URL, "Bearer "+guestToken[:len(guestToken)-1]+last, 401, "", nil)
	if !reflect.DeepEqual(rejected, []string{"unknown bearer token", "malformed bearer token"}) {
		t.Errorf("rejections reported: %v", rejected)
	}

	// On the tailnet, a token admits even a tagged node, which has no
	// login of its own.
	whois := func(ctx context.Context, remoteAddr string) (auth.Identity, error) {
		return auth.Identity{Login: "tagged-devices", Tagged: true}, nil
	}
	tailnet := httptest.NewServer(auth.Bearer(m.dir, reject, auth.Tailscale(m.dir, whois, reject, m.srv)))
	t.Cleanup(tailnet.Close)
	checkAuthorized(t, tailnet.URL, "Bearer "+guestToken, 200, "guest", []string{"guest"})
	checkAuthorized(t, tailnet.URL, "", 403, "", nil)
}
