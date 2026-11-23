package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/rjbs/taskmaster/internal/auth"
	"github.com/rjbs/taskmaster/internal/jmap"
	"github.com/rjbs/taskmaster/internal/store"
	"github.com/rjbs/taskmaster/internal/tasks"

	taskblaster "github.com/rjbs/taskmaster/demo-client"
)

const account = "rjbs"

type obj = map[string]any

// env is a running server with an in-memory database.
type env struct {
	t      *testing.T
	ts     *httptest.Server
	broker *Broker
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, nil)
}

// newEnvWith is newEnv with a page to serve at the root, or none.
func newEnvWith(t *testing.T, page http.Handler) *env {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	loc, _ := time.LoadLocation("America/New_York")
	svc := tasks.New(db, tasks.Config{AccountID: account, DefaultTimeZone: loc})
	if err := svc.EnsureDefaultTaskList(t.Context()); err != nil {
		t.Fatal(err)
	}
	reg := tasks.NewRegistry()
	reg.Add(svc)
	proc := jmap.NewProcessor()
	reg.Register(proc)
	broker := NewBroker()
	db.OnCommit(broker.Publisher(account))

	dir, err := auth.NewDirectory(
		map[string]*auth.User{account: {Credentials: []auth.Credential{{Type: auth.CredLoopback}}}},
		map[string]*auth.Account{account: {Name: account, Owner: account}},
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Config{Directory: dir, Accounts: reg, Processor: proc, Broker: broker, Page: page})
	ts := httptest.NewServer(auth.Loopback(dir, srv))
	t.Cleanup(ts.Close)
	return &env{t: t, ts: ts, broker: broker}
}

// request posts method calls and returns the raw method responses.
func (e *env) request(calls ...jmap.Invocation) []jmap.Invocation {
	e.t.Helper()
	body, _ := json.Marshal(jmap.Request{
		Using:       []string{jmap.CoreCapability, tasks.Capability},
		MethodCalls: calls,
	})
	resp, err := http.Post(e.ts.URL+"/jmap", "application/json", bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		e.t.Fatalf("POST /jmap: status %d", resp.StatusCode)
	}
	// Decode with plain encoding/json rather than through jmap.Response,
	// whose decoder keeps numbers as json.Number; tests want float64.
	var out struct {
		MethodResponses [][3]json.RawMessage `json:"methodResponses"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		e.t.Fatal(err)
	}
	var invs []jmap.Invocation
	for _, r := range out.MethodResponses {
		var inv jmap.Invocation
		json.Unmarshal(r[0], &inv.Name)
		json.Unmarshal(r[1], &inv.Args)
		json.Unmarshal(r[2], &inv.CallID)
		invs = append(invs, inv)
	}
	return invs
}

func withAccount(args obj) obj {
	out := obj{"accountId": account}
	for k, v := range args {
		out[k] = v
	}
	return out
}

// call runs one method and fails the test if it returns an error.
func (e *env) call(name string, args obj) obj {
	e.t.Helper()
	resps := e.request(jmap.Invocation{Name: name, Args: withAccount(args), CallID: "c"})
	if resps[0].Name == "error" {
		e.t.Fatalf("%s returned error: %v", name, resps[0].Args)
	}
	return resps[0].Args
}

// callError runs one method and returns the error type it produced.
func (e *env) callError(name string, args obj) string {
	e.t.Helper()
	resps := e.request(jmap.Invocation{Name: name, Args: withAccount(args), CallID: "c"})
	if resps[0].Name != "error" {
		e.t.Fatalf("%s succeeded; expected an error", name)
	}
	return resps[0].Args["type"].(string)
}

func (e *env) createList(name string, extra obj) string {
	e.t.Helper()
	props := obj{"name": name}
	for k, v := range extra {
		props[k] = v
	}
	resp := e.call("TaskList/set", obj{"create": obj{"new": props}})
	created, _ := resp["created"].(obj)
	if created["new"] == nil {
		e.t.Fatalf("TaskList/set did not create %q: %v", name, resp["notCreated"])
	}
	return created["new"].(obj)["id"].(string)
}

func (e *env) createTask(listID string, props obj) string {
	e.t.Helper()
	full := obj{"taskListIds": obj{listID: true}}
	for k, v := range props {
		full[k] = v
	}
	resp := e.call("Task/set", obj{"create": obj{"new": full}})
	created, _ := resp["created"].(obj)
	if created["new"] == nil {
		e.t.Fatalf("Task/set did not create %v: %v", props, resp["notCreated"])
	}
	return created["new"].(obj)["id"].(string)
}

func (e *env) getTask(id string) obj {
	e.t.Helper()
	resp := e.call("Task/get", obj{"ids": []string{id}})
	list := resp["list"].([]any)
	if len(list) != 1 {
		e.t.Fatalf("Task/get %s: not found", id)
	}
	return list[0].(obj)
}

func (e *env) getList(id string) obj {
	e.t.Helper()
	resp := e.call("TaskList/get", obj{"ids": []string{id}})
	list := resp["list"].([]any)
	if len(list) != 1 {
		e.t.Fatalf("TaskList/get %s: not found", id)
	}
	return list[0].(obj)
}

func (e *env) defaultListID() string {
	e.t.Helper()
	for _, l := range e.call("TaskList/get", obj{})["list"].([]any) {
		if l.(obj)["isDefault"] == true {
			return l.(obj)["id"].(string)
		}
	}
	e.t.Fatal("no default task list")
	return ""
}

// setErrorProps returns the "properties" of the SetError for key in the
// given not* map of a /set response.
func setErrorProps(t *testing.T, resp obj, which, key string) (string, []string) {
	t.Helper()
	m, _ := resp[which].(obj)
	se, ok := m[key].(obj)
	if !ok {
		t.Fatalf("expected a SetError in %s for %s; got %v", which, key, resp)
	}
	var props []string
	if list, ok := se["properties"].([]any); ok {
		for _, p := range list {
			props = append(props, p.(string))
		}
	}
	sort.Strings(props)
	return se["type"].(string), props
}

func TestSession(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Get(e.ts.URL + "/.well-known/jmap")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sess obj
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		t.Fatal(err)
	}
	if sess["username"] != account {
		t.Errorf("username = %v", sess["username"])
	}
	if _, ok := sess["capabilities"].(obj)[tasks.Capability]; !ok {
		t.Error("tasks capability missing from session")
	}
	acct := sess["accounts"].(obj)[account].(obj)
	caps := acct["accountCapabilities"].(obj)[tasks.Capability].(obj)
	if caps["mayCreateTaskList"] != true {
		t.Error("mayCreateTaskList should be true")
	}
	if sess["apiUrl"] != e.ts.URL+"/jmap" {
		t.Errorf("apiUrl = %v", sess["apiUrl"])
	}
}

func TestDefaultTaskListSeeded(t *testing.T) {
	e := newEnv(t)
	lists := e.call("TaskList/get", obj{})["list"].([]any)
	if len(lists) != 1 || lists[0].(obj)["name"] != "Tasks" || lists[0].(obj)["isDefault"] != true {
		t.Errorf("expected one default list named Tasks, got %v", lists)
	}
}

func TestTaskListLifecycle(t *testing.T) {
	e := newEnv(t)
	id := e.createList("Groceries", obj{"color": "Teal", "sortOrder": 3})

	l := e.getList(id)
	if l["name"] != "Groceries" || l["color"] != "Teal" || l["sortOrder"] != 3.0 || l["isSubscribed"] != true || l["isDefault"] != false {
		t.Errorf("unexpected list after create: %v", l)
	}

	e.call("TaskList/set", obj{"update": obj{id: obj{"name": "Food", "color": nil, "sortOrder": nil}}})
	l = e.getList(id)
	if l["name"] != "Food" || l["color"] != nil || l["sortOrder"] != 0.0 {
		t.Errorf("unexpected list after update: %v", l)
	}

	resp := e.call("TaskList/set", obj{"destroy": []string{id}})
	if !reflect.DeepEqual(resp["destroyed"], []any{id}) {
		t.Errorf("destroy failed: %v", resp)
	}
	got := e.call("TaskList/get", obj{"ids": []string{id}})
	if !reflect.DeepEqual(got["notFound"], []any{id}) {
		t.Errorf("destroyed list still found: %v", got)
	}
}

func TestTaskListSetDefault(t *testing.T) {
	e := newEnv(t)
	oldDefault := e.defaultListID()

	resp := e.call("TaskList/set", obj{
		"create":                obj{"w": obj{"name": "Work"}},
		"onSuccessSetIsDefault": "#w",
	})
	created := resp["created"].(obj)["w"].(obj)
	if created["isDefault"] != true {
		t.Errorf("created list should report isDefault true: %v", created)
	}
	if upd := resp["updated"].(obj)[oldDefault]; !reflect.DeepEqual(upd, obj{"isDefault": false}) {
		t.Errorf("old default should be reported updated: %v", resp["updated"])
	}
	if e.defaultListID() != created["id"] {
		t.Error("default did not move")
	}

	resp = e.call("TaskList/set", obj{"update": obj{oldDefault: obj{"isDefault": true}}})
	if typ, props := setErrorProps(t, resp, "notUpdated", oldDefault); typ != "invalidProperties" || !slices.Equal(props, []string{"isDefault"}) {
		t.Errorf("setting isDefault directly should fail: %v", resp)
	}
}

func TestTaskListDestroyWithTasks(t *testing.T) {
	e := newEnv(t)
	keep := e.createList("Keep", nil)
	doomed := e.createList("Doomed", nil)
	only := e.createTask(doomed, obj{"title": "only in doomed"})
	both := e.createTask(doomed, obj{"title": "in both", "taskListIds": obj{doomed: true, keep: true}})

	resp := e.call("TaskList/set", obj{"destroy": []string{doomed}})
	if typ, _ := setErrorProps(t, resp, "notDestroyed", doomed); typ != tasks.SetErrTaskListHasTask {
		t.Errorf("expected taskListHasTask, got %v", resp)
	}

	resp = e.call("TaskList/set", obj{"destroy": []string{doomed}, "onDestroyRemoveTasks": true})
	if !reflect.DeepEqual(resp["destroyed"], []any{doomed}) {
		t.Fatalf("destroy failed: %v", resp)
	}
	got := e.call("Task/get", obj{"ids": []string{only, both}})
	if !reflect.DeepEqual(got["notFound"], []any{only}) {
		t.Errorf("task only in the destroyed list should be gone: %v", got)
	}
	list := got["list"].([]any)
	if len(list) != 1 || !reflect.DeepEqual(list[0].(obj)["taskListIds"], obj{keep: true}) {
		t.Errorf("task in both lists should remain only in Keep: %v", list)
	}
}

// checkListInvalid asserts that creating a list with props fails with an
// invalidProperties SetError naming exactly wantProps.
func checkListInvalid(t *testing.T, e *env, props obj, wantProps ...string) {
	t.Helper()
	resp := e.call("TaskList/set", obj{"create": obj{"x": props}})
	typ, got := setErrorProps(t, resp, "notCreated", "x")
	sort.Strings(wantProps)
	if typ != "invalidProperties" || !slices.Equal(got, wantProps) {
		t.Errorf("create %v: got %s %v, want invalidProperties %v", props, typ, got, wantProps)
	}
}

func TestTaskListValidation(t *testing.T) {
	e := newEnv(t)
	checkListInvalid(t, e, obj{}, "name")
	checkListInvalid(t, e, obj{"name": ""}, "name")
	checkListInvalid(t, e, obj{"name": "x", "color": "not-a-color"}, "color")
	checkListInvalid(t, e, obj{"name": "x", "sortOrder": -1}, "sortOrder")
	checkListInvalid(t, e, obj{"name": "x", "sortOrder": 1 << 31}, "sortOrder")
	checkListInvalid(t, e, obj{"name": "x", "timeZone": "Mars/Olympus"}, "timeZone")
	checkListInvalid(t, e, obj{"name": "x", "isDefault": true}, "isDefault")
	checkListInvalid(t, e, obj{"name": "x", "id": "L-1"}, "id")
	checkListInvalid(t, e, obj{"name": "x", "shareWith": obj{"p": obj{}}}, "shareWith")
	checkListInvalid(t, e, obj{"name": "x", "bogus": 1}, "bogus")
	checkListInvalid(t, e, obj{"name": "x", "nodomain:project": 1}, "nodomain:project")
	checkListInvalid(t, e, obj{"name": "x", "example.com:": 1}, "example.com:")
	checkListInvalid(t, e, obj{"name": "x", "-bad.com:p": 1}, "-bad.com:p")
	checkListInvalid(t, e, obj{"name": "x", "defaultAlertsWithTime": obj{
		"a1": obj{"@type": "Alert", "trigger": obj{"@type": "AbsoluteTrigger", "when": "2026-01-01T00:00:00Z"}},
	}}, "defaultAlertsWithTime")
}

func TestTaskLifecycle(t *testing.T) {
	e := newEnv(t)
	resps := e.request(
		jmap.Invocation{Name: "TaskList/set", CallID: "0", Args: withAccount(obj{
			"create": obj{"l": obj{"name": "Errands"}},
		})},
		jmap.Invocation{Name: "Task/set", CallID: "1", Args: withAccount(obj{
			"create": obj{"k1": obj{
				"taskListIds": obj{"#l": true},
				"title":       "Renew passport",
				"due":         "2026-09-01T09:00:00",
				"timeZone":    "Australia/Melbourne",
				"priority":    3,
				"keywords":    obj{"travel": true},
			}},
		})},
		jmap.Invocation{Name: "Task/get", CallID: "2", Args: withAccount(obj{
			"ids": []string{"#k1"},
		})},
	)
	for _, r := range resps {
		if r.Name == "error" {
			t.Fatalf("call %s failed: %v", r.CallID, r.Args)
		}
	}
	listID := resps[0].Args["created"].(obj)["l"].(obj)["id"].(string)
	created := resps[1].Args["created"].(obj)["k1"].(obj)
	for _, p := range []string{"id", "version", "uid", "created", "updated"} {
		if created[p] == nil {
			t.Errorf("created response lacks %s: %v", p, created)
		}
	}
	taskID := created["id"].(string)

	fetched := resps[2].Args["list"].([]any)[0].(obj)
	if fetched["title"] != "Renew passport" || fetched["@type"] != "Task" || fetched["version"] != "2.0" || fetched["priority"] != 3.0 {
		t.Errorf("unexpected task: %v", fetched)
	}
	if !reflect.DeepEqual(fetched["taskListIds"], obj{listID: true}) {
		t.Errorf("taskListIds = %v", fetched["taskListIds"])
	}

	resp := e.call("Task/set", obj{"update": obj{taskID: obj{
		"progress":        "completed",
		"percentComplete": 100,
		"keywords/urgent": true,
		"priority":        nil,
	}}})
	if resp["updated"].(obj)[taskID] == nil {
		t.Fatalf("update failed: %v", resp)
	}
	task := e.getTask(taskID)
	if task["progress"] != "completed" || task["percentComplete"] != 100.0 {
		t.Errorf("progress not updated: %v", task)
	}
	if _, has := task["priority"]; has {
		t.Error("priority should have been removed")
	}
	if !reflect.DeepEqual(task["keywords"], obj{"travel": true, "urgent": true}) {
		t.Errorf("keywords = %v", task["keywords"])
	}
	if task["updated"] == created["updated"] && task["updated"].(string) < created["updated"].(string) {
		t.Error("updated should not go backwards")
	}

	resp = e.call("Task/set", obj{"update": obj{taskID: obj{"id": "T-other"}}})
	if typ, _ := setErrorProps(t, resp, "notUpdated", taskID); typ != "invalidProperties" {
		t.Errorf("changing id should fail: %v", resp)
	}

	props := e.call("Task/get", obj{"ids": []string{taskID}, "properties": []string{"title"}})["list"].([]any)[0].(obj)
	if len(props) != 2 || props["id"] != taskID || props["title"] != "Renew passport" {
		t.Errorf("properties filter: %v", props)
	}

	resp = e.call("Task/set", obj{"destroy": []string{taskID}})
	if !reflect.DeepEqual(resp["destroyed"], []any{taskID}) {
		t.Errorf("destroy failed: %v", resp)
	}
}

// checkTaskInvalid asserts that creating a task with props (in the default
// list unless props says otherwise) fails naming exactly wantProps.
func checkTaskInvalid(t *testing.T, e *env, props obj, wantProps ...string) {
	t.Helper()
	full := obj{"taskListIds": obj{e.defaultListID(): true}}
	for k, v := range props {
		full[k] = v
	}
	resp := e.call("Task/set", obj{"create": obj{"x": full}})
	typ, got := setErrorProps(t, resp, "notCreated", "x")
	sort.Strings(wantProps)
	if typ != "invalidProperties" || !slices.Equal(got, wantProps) {
		t.Errorf("create %v: got %s %v, want invalidProperties %v", props, typ, got, wantProps)
	}
}

func TestTaskValidation(t *testing.T) {
	e := newEnv(t)
	listID := e.defaultListID()

	// A plain undated task is fine; one with a zone but no date is not.
	e.createTask(listID, obj{"title": "buy milk"})
	checkTaskInvalid(t, e, obj{"title": "x", "timeZone": "Europe/Paris"}, "due")
	checkTaskInvalid(t, e, obj{"title": "x", "showWithoutTime": true}, "due")

	checkTaskInvalid(t, e, obj{"taskListIds": obj{}}, "taskListIds")
	checkTaskInvalid(t, e, obj{"taskListIds": obj{"L-nope": true}}, "taskListIds")
	checkTaskInvalid(t, e, obj{"taskListIds": obj{listID: false}}, "taskListIds")
	checkTaskInvalid(t, e, obj{"@type": "Event"}, "@type")
	checkTaskInvalid(t, e, obj{"method": "REQUEST"}, "method")
	checkTaskInvalid(t, e, obj{"id": "T-1"}, "id")
	checkTaskInvalid(t, e, obj{"due": "2026-09-01T09:00:00Z"}, "due")
	checkTaskInvalid(t, e, obj{"due": "tomorrow"}, "due")
	checkTaskInvalid(t, e, obj{"start": "2026-09-01T09:00:00", "timeZone": "Nowhere/Nope"}, "timeZone")
	checkTaskInvalid(t, e, obj{"percentComplete": 101}, "percentComplete")
	checkTaskInvalid(t, e, obj{"priority": 10}, "priority")
	checkTaskInvalid(t, e, obj{"estimatedDuration": "2 hours"}, "estimatedDuration")
	checkTaskInvalid(t, e, obj{"recurrenceRule": obj{"frequency": "weekly"}}, "start")
	checkTaskInvalid(t, e, obj{"keywords": obj{"a": false}}, "keywords")
	checkTaskInvalid(t, e, obj{"uid": ""}, "uid")
	checkTaskInvalid(t, e, obj{"version": "1.0"}, "version")
	checkTaskInvalid(t, e, obj{"version": "2.1"}, "version")
	checkTaskInvalid(t, e, obj{"version": 2.0}, "version")
	e.createTask(listID, obj{"title": "explicitly current", "version": "2.0"})

	// Creating with a uid already in use is reported as alreadyExists with
	// the id of the existing task, so a retried create can recover it.
	// Moving an existing task onto a used uid is just an invalid patch.
	first := e.createTask(listID, obj{"uid": "dup", "title": "first"})
	resp := e.call("Task/set", obj{"create": obj{"x": obj{"taskListIds": obj{listID: true}, "uid": "dup", "title": "second"}}})
	se, _ := resp["notCreated"].(obj)["x"].(obj)
	if se["type"] != "alreadyExists" || se["existingId"] != first {
		t.Errorf("duplicate uid on create: %v", resp["notCreated"])
	}
	other := e.createTask(listID, obj{"uid": "other", "title": "third"})
	resp = e.call("Task/set", obj{"update": obj{other: obj{"uid": "dup"}}})
	if typ, props := setErrorProps(t, resp, "notUpdated", other); typ != "invalidProperties" || !slices.Equal(props, []string{"uid"}) {
		t.Errorf("duplicate uid on update: %v", resp["notUpdated"])
	}
	resp = e.call("Task/set", obj{"update": obj{other: obj{"version": nil}}})
	if typ, props := setErrorProps(t, resp, "notUpdated", other); typ != "invalidProperties" || !slices.Equal(props, []string{"version"}) {
		t.Errorf("removing version: %v", resp["notUpdated"])
	}

	// A different recurrenceId makes it a different task, not a duplicate.
	e.createTask(listID, obj{"uid": "dup", "title": "instance", "start": "2026-10-04T09:00:00", "recurrenceId": "2026-10-04T09:00:00"})
}

// offsetAlert is an alerts map holding one OffsetTrigger alert, relative
// to relativeTo, or to the default if relativeTo is "".
func offsetAlert(relativeTo string) obj {
	trigger := obj{"@type": "OffsetTrigger", "offset": "-PT15M"}
	if relativeTo != "" {
		trigger["relativeTo"] = relativeTo
	}
	return obj{"a1": obj{"@type": "Alert", "trigger": trigger}}
}

func TestAlertAnchors(t *testing.T) {
	e := newEnv(t)
	listID := e.defaultListID()
	const day = "2026-10-05T09:00:00"
	absolute := obj{"a1": obj{"@type": "Alert", "trigger": obj{"@type": "AbsoluteTrigger", "when": "2026-10-05T08:00:00Z"}}}

	checkTaskInvalid(t, e, obj{"alerts": offsetAlert("")}, "alerts")
	checkTaskInvalid(t, e, obj{"alerts": offsetAlert("start"), "due": day}, "alerts")
	checkTaskInvalid(t, e, obj{"alerts": offsetAlert("end"), "start": day}, "alerts")
	e.createTask(listID, obj{"title": "default anchor", "alerts": offsetAlert(""), "start": day})
	e.createTask(listID, obj{"title": "end anchor", "alerts": offsetAlert("end"), "due": day})
	e.createTask(listID, obj{"title": "absolute", "alerts": absolute})

	weekly := obj{"frequency": "weekly"}
	const next = "2026-10-12T09:00:00"
	checkTaskInvalid(t, e, obj{"start": day, "due": day, "recurrenceRule": weekly, "alerts": offsetAlert("end"),
		"recurrenceOverrides": obj{next: obj{"due": nil}}}, "recurrenceOverrides")
	checkTaskInvalid(t, e, obj{"start": day, "recurrenceRule": weekly,
		"recurrenceOverrides": obj{next: obj{"alerts": offsetAlert("end")}}}, "recurrenceOverrides")
	checkTaskInvalid(t, e, obj{"start": day, "recurrenceRule": weekly, "alerts": offsetAlert(""),
		"recurrenceOverrides": obj{next: obj{"start": nil}}}, "recurrenceOverrides")
	// An instance's start comes from its recurrence id, so it always has
	// one unless the patch removes it; an excluded instance has no alerts.
	e.createTask(listID, obj{"title": "weekly", "start": day, "recurrenceRule": weekly,
		"recurrenceOverrides": obj{next: obj{"alerts": offsetAlert("start")}}})
	e.createTask(listID, obj{"title": "skipped", "start": day, "due": day, "recurrenceRule": weekly, "alerts": offsetAlert("end"),
		"recurrenceOverrides": obj{next: obj{"excluded": true}}})

	// Removing the anchor later is caught too.
	id := e.createTask(listID, obj{"title": "due soon", "alerts": offsetAlert("end"), "due": day})
	resp := e.call("Task/set", obj{"update": obj{id: obj{"due": nil}}})
	if typ, props := setErrorProps(t, resp, "notUpdated", id); typ != "invalidProperties" || !slices.Equal(props, []string{"alerts"}) {
		t.Errorf("removing due under an end alert: %v", resp["notUpdated"])
	}
}

func TestReservedProperties(t *testing.T) {
	e := newEnv(t)
	listID := e.defaultListID()

	for _, prop := range []string{"localizations", "replyTo", "requestStatus", "excluded", "extra"} {
		checkTaskInvalid(t, e, obj{prop: obj{}}, prop)
	}
	checkTaskInvalid(t, e, obj{"participants": obj{"p1": obj{"@type": "Participant", "name": "Ann", "sendTo": obj{}}}},
		"participants/p1/sendTo")
	checkTaskInvalid(t, e, obj{"locations": obj{"l1": obj{"@type": "Location", "name": "Harbour", "description": "by the lighthouse"}}},
		"locations/l1/description")
	checkTaskInvalid(t, e, obj{"virtualLocations": obj{"v1": obj{"@type": "VirtualLocation", "uri": "https://example.com/", "description": "call"}}},
		"virtualLocations/v1/description")
	checkTaskInvalid(t, e, obj{"links": obj{"a/b": obj{"@type": "Link", "href": "https://example.com/", "extra": 1}}},
		"links/a~1b/extra")
	checkTaskInvalid(t, e, obj{"localizations": obj{}, "replyTo": obj{}}, "localizations", "replyTo")

	// Reserved names are only properties; as data they mean nothing.
	e.createTask(listID, obj{"title": "keywords", "keywords": obj{"extra": true, "localizations": true}})

	weekly := obj{"frequency": "weekly"}
	const day, next = "2026-10-05T09:00:00", "2026-10-12T09:00:00"
	checkTaskInvalid(t, e, obj{"start": day, "recurrenceRule": weekly,
		"recurrenceOverrides": obj{next: obj{"localizations": obj{}}}}, "recurrenceOverrides")
	checkTaskInvalid(t, e, obj{"start": day, "recurrenceRule": weekly,
		"recurrenceOverrides": obj{next: obj{"excluded": true, "title": "skipped?"}}}, "recurrenceOverrides")
	checkTaskInvalid(t, e, obj{"start": day, "recurrenceRule": obj{"frequency": "weekly", "extra": 1}},
		"recurrenceRule/extra")
	e.createTask(listID, obj{"title": "skips one", "start": day, "recurrenceRule": weekly,
		"recurrenceOverrides": obj{next: obj{"excluded": true}}})
}

// checkQuery runs Task/query in America/New_York with filter and sort and
// checks the titles of the results, in order.
func checkQuery(t *testing.T, e *env, filter any, sortBy []obj, wantTitles ...string) {
	t.Helper()
	checkQueryInZone(t, e, "America/New_York", filter, sortBy, wantTitles...)
}

func checkQueryInZone(t *testing.T, e *env, tz string, filter any, sortBy []obj, wantTitles ...string) {
	t.Helper()
	args := obj{"timeZone": tz}
	if filter != nil {
		args["filter"] = filter
	}
	if sortBy != nil {
		args["sort"] = sortBy
	}
	ids := e.call("Task/query", args)["ids"].([]any)
	var titles []string
	for _, id := range ids {
		titles = append(titles, e.getTask(id.(string))["title"].(string))
	}
	if wantTitles == nil {
		wantTitles = []string{}
	}
	if titles == nil {
		titles = []string{}
	}
	if !slices.Equal(titles, wantTitles) {
		t.Errorf("query filter=%v sort=%v: got %v, want %v", filter, sortBy, titles, wantTitles)
	}
}

// checkIDSort runs Task/query sorted on id in the given direction and
// asserts it returns wantCount ids, in that order.  Ids are server-chosen,
// so the expected order is derived from the ids themselves.
func checkIDSort(t *testing.T, e *env, ascending bool, wantCount int) {
	t.Helper()
	resp := e.call("Task/query", obj{"sort": []obj{{"property": "id", "isAscending": ascending}}})
	var ids []string
	for _, id := range resp["ids"].([]any) {
		ids = append(ids, id.(string))
	}
	want := slices.Clone(ids)
	slices.Sort(want)
	if !ascending {
		slices.Reverse(want)
	}
	if len(ids) != wantCount || !slices.Equal(ids, want) {
		t.Errorf("sort on id (ascending=%v): got %v", ascending, ids)
	}
}

func TestTaskQuery(t *testing.T) {
	e := newEnv(t)
	home := e.defaultListID()
	work := e.createList("Work", nil)

	e.createTask(home, obj{"title": "Water the plants", "due": "2026-10-05T09:00:00", "priority": 5})
	e.createTask(home, obj{"title": "Buy bus tickets", "due": "2026-10-03T18:00:00", "progress": "in-process", "priority": 1})
	e.createTask(work, obj{"title": "File expenses", "due": "2026-10-10T17:00:00", "description": "Receipts from the Melbourne trip", "progress": "completed", "percentComplete": 100})
	e.createTask(work, obj{"title": "Plan offsite", "start": "2026-10-01T09:00:00", "due": "2026-10-20T17:00:00", "keywords": obj{"planning": true}})
	e.createTask(home, obj{"title": "Someday: learn Go"})
	e.createTask(home, obj{"title": "Early bird", "due": "2026-10-04T08:00:00", "timeZone": "Europe/London"})

	byDue := []obj{{"property": "due"}}
	byDueDesc := []obj{{"property": "due", "isAscending": false}}

	checkQuery(t, e, nil, byDue,
		"Buy bus tickets", "Early bird", "Water the plants", "File expenses", "Plan offsite", "Someday: learn Go")
	checkQuery(t, e, nil, byDueDesc,
		"Plan offsite", "File expenses", "Water the plants", "Early bird", "Buy bus tickets", "Someday: learn Go")
	checkQuery(t, e, obj{"inTaskList": work}, byDue, "File expenses", "Plan offsite")
	checkQuery(t, e, obj{"progress": []string{"needs-action", "in-process"}}, byDue,
		"Buy bus tickets", "Early bird", "Water the plants", "Plan offsite", "Someday: learn Go")
	checkQuery(t, e, obj{"progress": []string{}}, byDue)
	checkQuery(t, e, obj{"after": "2026-10-04T00:00:00", "before": "2026-10-06T00:00:00"}, byDue,
		"Early bird", "Water the plants", "Plan offsite")
	checkQuery(t, e, obj{"before": "2026-10-04T05:00:00"}, byDue, "Buy bus tickets", "Early bird", "Plan offsite")
	// Early bird is due 08:00 London (07:00Z).  Seen from New York the
	// 05:00 bound is 09:00Z and it is included; seen from London the bound
	// is 04:00Z and it is not.  The zoneless tasks move with the query.
	checkQueryInZone(t, e, "Europe/London", obj{"before": "2026-10-04T05:00:00"}, byDue, "Buy bus tickets", "Plan offsite")
	checkQueryInZone(t, e, "Europe/London", nil, byDue,
		"Buy bus tickets", "Early bird", "Water the plants", "File expenses", "Plan offsite", "Someday: learn Go")
	// The same local window read in two zones: from New York it is
	// 04:00Z to 16:00Z on the 4th and catches Early bird (07:00Z); from
	// Kiritimati (UTC+14) it is 10:00Z to 22:00Z on the 3rd and does not.
	window := obj{"after": "2026-10-04T00:00:00", "before": "2026-10-04T12:00:00"}
	checkQueryInZone(t, e, "America/New_York", window, byDue, "Early bird", "Plan offsite")
	checkQueryInZone(t, e, "Pacific/Kiritimati", window, byDue, "Plan offsite")
	checkQuery(t, e, obj{"text": "plant"}, byDue, "Water the plants")
	checkQuery(t, e, obj{"text": "receipt"}, byDue, "File expenses")
	checkQuery(t, e, obj{"text": "MELBOURNE"}, byDue, "File expenses")
	checkQuery(t, e, obj{"title": "melbourne"}, byDue)
	checkQuery(t, e, obj{"description": "\"melbourne trip\""}, byDue, "File expenses")
	checkQuery(t, e, obj{"text": "planning"}, byDue, "Plan offsite")
	checkQuery(t, e, obj{"operator": "OR", "conditions": []obj{{"inTaskList": work}, {"text": "plants"}}}, byDue,
		"Water the plants", "File expenses", "Plan offsite")
	checkQuery(t, e, obj{"operator": "NOT", "conditions": []obj{{"inTaskList": work}}}, byDue,
		"Buy bus tickets", "Early bird", "Water the plants", "Someday: learn Go")
	checkQuery(t, e, nil, []obj{{"property": "priority"}, {"property": "due"}},
		"Buy bus tickets", "Water the plants", "Early bird", "File expenses", "Plan offsite", "Someday: learn Go")
	checkQuery(t, e, nil, []obj{{"property": "progress", "isAscending": false}, {"property": "due"}},
		"File expenses", "Buy bus tickets", "Early bird", "Water the plants", "Plan offsite", "Someday: learn Go")

	resp := e.call("Task/query", obj{"sort": byDue, "position": 1, "limit": 2, "calculateTotal": true})
	if resp["total"] != 6.0 || resp["position"] != 1.0 || len(resp["ids"].([]any)) != 2 {
		t.Errorf("paging: %v", resp)
	}
	resp = e.call("Task/query", obj{"sort": byDue, "position": -2})
	if resp["position"] != 4.0 || len(resp["ids"].([]any)) != 2 {
		t.Errorf("negative position: %v", resp)
	}

	if typ := e.callError("Task/query", obj{"sort": []obj{{"property": "title"}}}); typ != "unsupportedSort" {
		t.Errorf("sort on title: %s", typ)
	}
	checkIDSort(t, e, true, 6)
	checkIDSort(t, e, false, 6)

	if typ := e.callError("Task/query", obj{"sort": []obj{{"property": "recurrenceId"}}}); typ != "unsupportedSort" {
		t.Errorf("sort on recurrenceId: %s", typ)
	}
	if typ := e.callError("Task/query", obj{"filter": obj{"color": "red"}}); typ != "unsupportedFilter" {
		t.Errorf("filter on color: %s", typ)
	}
	if typ := e.callError("Task/query", obj{"expandRecurrences": true, "filter": obj{"after": "2026-01-01T00:00:00", "before": "2026-02-01T00:00:00"}}); typ != tasks.ErrCannotCalculateOccurrences {
		t.Errorf("expandRecurrences: %s", typ)
	}
}

func TestChanges(t *testing.T) {
	e := newEnv(t)
	listID := e.defaultListID()
	state0 := e.call("Task/get", obj{"ids": []string{}})["state"].(string)

	a := e.createTask(listID, obj{"title": "a"})
	b := e.createTask(listID, obj{"title": "b"})
	state1 := e.call("Task/get", obj{"ids": []string{}})["state"].(string)

	e.call("Task/set", obj{"update": obj{a: obj{"title": "a2"}}})
	e.call("Task/set", obj{"destroy": []string{b}})
	c := e.createTask(listID, obj{"title": "c"})
	e.call("Task/set", obj{"destroy": []string{c}})

	checkChanges(t, e, "Task", state0, []string{a}, nil, nil)
	checkChanges(t, e, "Task", state1, nil, []string{a}, []string{b})

	resp := e.call("Task/changes", obj{"sinceState": state0, "maxChanges": 1})
	if resp["hasMoreChanges"] != true {
		t.Errorf("expected hasMoreChanges with maxChanges 1: %v", resp)
	}
	resp = e.call("Task/changes", obj{"sinceState": resp["newState"]})
	if resp["hasMoreChanges"] != false {
		t.Errorf("second page should finish: %v", resp)
	}

	if typ := e.callError("Task/changes", obj{"sinceState": "999999"}); typ != "cannotCalculateChanges" {
		t.Errorf("future state: %s", typ)
	}
	if typ := e.callError("Task/set", obj{"ifInState": state0, "destroy": []string{a}}); typ != "stateMismatch" {
		t.Errorf("stale ifInState: %s", typ)
	}
	if typ := e.callError("Task/queryChanges", obj{"sinceQueryState": state0}); typ != "cannotCalculateChanges" {
		t.Errorf("queryChanges: %s", typ)
	}
}

// checkChanges compares a /changes response against the expected id sets,
// ignoring order within each set.
func checkChanges(t *testing.T, e *env, typ, since string, created, updated, destroyed []string) {
	t.Helper()
	resp := e.call(typ+"/changes", obj{"sinceState": since})
	for name, want := range map[string][]string{"created": created, "updated": updated, "destroyed": destroyed} {
		var got []string
		for _, id := range resp[name].([]any) {
			got = append(got, id.(string))
		}
		sort.Strings(got)
		sort.Strings(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s/changes since %s: %s = %v, want %v", typ, since, name, got, want)
		}
	}
}

func TestNotificationsAreEmpty(t *testing.T) {
	e := newEnv(t)
	resp := e.call("TaskNotification/get", obj{"ids": []string{"n1"}})
	if len(resp["list"].([]any)) != 0 || !reflect.DeepEqual(resp["notFound"], []any{"n1"}) {
		t.Errorf("get: %v", resp)
	}
	resp = e.call("TaskNotification/query", obj{})
	if len(resp["ids"].([]any)) != 0 {
		t.Errorf("query: %v", resp)
	}
	resp = e.call("TaskNotification/set", obj{"create": obj{"x": obj{}}})
	if typ, _ := setErrorProps(t, resp, "notCreated", "x"); typ != "forbidden" {
		t.Errorf("set create: %v", resp)
	}
}

func TestRequestErrors(t *testing.T) {
	e := newEnv(t)
	post := func(body string) (int, string) {
		resp, err := http.Post(e.ts.URL+"/jmap", "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var pd obj
		json.NewDecoder(resp.Body).Decode(&pd)
		typ, _ := pd["type"].(string)
		return resp.StatusCode, typ
	}
	if status, typ := post(`{not json`); status != 400 || typ != jmap.ReqErrNotJSON {
		t.Errorf("not JSON: %d %s", status, typ)
	}
	if status, typ := post(`{"using": []}`); status != 400 || typ != jmap.ReqErrNotRequest {
		t.Errorf("not a request: %d %s", status, typ)
	}
	if status, typ := post(fmt.Sprintf(`{"using": [%q], "methodCalls": []}`, tasks.Capability)); status != 400 || typ != jmap.ReqErrUnknownCapability {
		t.Errorf("missing core: %d %s", status, typ)
	}

	resps := e.request(jmap.Invocation{Name: "Task/get", CallID: "x", Args: obj{"accountId": "nobody"}})
	if resps[0].Name != "error" || resps[0].Args["type"] != "accountNotFound" {
		t.Errorf("wrong account: %v", resps[0])
	}
	resps = e.request(jmap.Invocation{Name: "Task/copy", CallID: "x", Args: withAccount(obj{})})
	if resps[0].Name != "error" || resps[0].Args["type"] != "unknownMethod" {
		t.Errorf("Task/copy: %v", resps[0])
	}
}

func TestEventSource(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Get(e.ts.URL + "/jmap/eventsource?types=Task&closeafter=state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}

	done := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		done <- string(buf[:n])
	}()

	// Give the subscriber a moment to register before we change anything.
	time.Sleep(50 * time.Millisecond)
	e.createList("Only lists changed", nil)
	e.createTask(e.defaultListID(), obj{"title": "poke"})

	select {
	case event := <-done:
		var payload obj
		lines := bytes.Split([]byte(event), []byte("\n"))
		for _, line := range lines {
			if bytes.HasPrefix(line, []byte("data: ")) {
				json.Unmarshal(bytes.TrimPrefix(line, []byte("data: ")), &payload)
			}
		}
		changed, _ := payload["changed"].(obj)
		acct, _ := changed[account].(obj)
		if payload["@type"] != "StateChange" || acct["Task"] == nil || acct["TaskList"] != nil {
			t.Errorf("unexpected event: %s", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no state event received")
	}
}

// TestVendorProperties walks through the NerfJournal encoding from that
// project's JMAP-SPEC.md: vendor-prefixed properties, colon-bearing keyword
// keys, a fixed-key link, and the patches its sweep and reopen flows send.
func TestVendorProperties(t *testing.T) {
	e := newEnv(t)
	const autoCancel = "nerfjournal.rjbs.cloud:autoCancel"
	const endedAt = "nerfjournal.rjbs.cloud:endedAt"
	const category = "keywords/nerfjournal:category:Work"

	id := e.createTask(e.defaultListID(), obj{
		"uid":             "9d2f5b1e-3e4a-4c5b-8f7e-1a2b3c4d5e6f",
		"title":           "Review the release notes",
		"start":           "2026-09-13T00:00:00",
		"showWithoutTime": true,
		autoCancel:        true,
		"keywords":        obj{"nerfjournal:category:Work": true},
		"links": obj{"nerfjournal-external": obj{
			"@type": "Link", "href": "https://example.com/pr/42", "rel": "related",
		}},
	})
	task := e.getTask(id)
	if task[autoCancel] != true || task["showWithoutTime"] != true {
		t.Errorf("vendor property not stored: %v", task)
	}
	if _, has := task["timeZone"]; has {
		t.Error("floating start should not acquire a timeZone")
	}

	// The sweep.
	resp := e.call("Task/set", obj{"update": obj{id: obj{
		"progress":        "cancelled",
		"progressUpdated": "2026-09-14T12:00:07Z",
		endedAt:           "2026-09-13",
	}}})
	if resp["updated"].(obj)[id] == nil {
		t.Fatalf("sweep update failed: %v", resp)
	}
	task = e.getTask(id)
	if task["progress"] != "cancelled" || task[endedAt] != "2026-09-13" {
		t.Errorf("sweep not applied: %v", task)
	}

	// Reopening, plus a category rename and a link change in the same patch.
	resp = e.call("Task/set", obj{"update": obj{id: obj{
		"progress":                           "needs-action",
		"progressUpdated":                    nil,
		endedAt:                              nil,
		category:                             nil,
		"keywords/nerfjournal:category:Home": true,
		"links/nerfjournal-external/href":    "https://example.com/pr/43",
	}}})
	if resp["updated"].(obj)[id] == nil {
		t.Fatalf("reopen update failed: %v", resp)
	}
	task = e.getTask(id)
	for _, p := range []string{endedAt, "progressUpdated"} {
		if _, has := task[p]; has {
			t.Errorf("%s should have been removed", p)
		}
	}
	if !reflect.DeepEqual(task["keywords"], obj{"nerfjournal:category:Home": true}) {
		t.Errorf("keywords = %v", task["keywords"])
	}
	if task["links"].(obj)["nerfjournal-external"].(obj)["href"] != "https://example.com/pr/43" {
		t.Errorf("links = %v", task["links"])
	}

	// Querying by the cancelled state and fetching only vendor properties.
	e.call("Task/set", obj{"update": obj{id: obj{"progress": "cancelled"}}})
	checkQuery(t, e, obj{"progress": []string{"cancelled"}}, nil, "Review the release notes")
	props := e.call("Task/get", obj{"ids": []string{id}, "properties": []string{autoCancel}})["list"].([]any)[0].(obj)
	if len(props) != 2 || props[autoCancel] != true {
		t.Errorf("vendor property filter: %v", props)
	}

	// RFC 8620 forbids patching below a parent that does not exist, so a
	// task with no keywords yet must be given the whole map.
	bare := e.createTask(e.defaultListID(), obj{"title": "foreign task"})
	resp = e.call("Task/set", obj{"update": obj{bare: obj{category: true}}})
	if typ, _ := setErrorProps(t, resp, "notUpdated", bare); typ != "invalidPatch" {
		t.Errorf("patch into missing keywords: %v", resp)
	}
	e.call("Task/set", obj{"update": obj{bare: obj{"keywords": obj{"nerfjournal:category:Work": true}}}})

	checkTaskInvalid(t, e, obj{"progressUpdated": "2026-09-13"}, "progressUpdated")
}

// TestTaskListVendorProperties covers the vendor-prefixed TaskList
// properties Loose Threads uses to keep a project's identity on its list.
func TestTaskListVendorProperties(t *testing.T) {
	e := newEnv(t)
	const project = "lt.semiotic.systems:project"
	const meta = "lt.semiotic.systems:meta"

	id := e.createList("Loose Threads", obj{project: "github.com/rjbs/Loose-Threads", meta: obj{"n": 1.0}})
	other := e.createList("Other", nil)
	l := e.getList(id)
	if l[project] != "github.com/rjbs/Loose-Threads" || !reflect.DeepEqual(l[meta], obj{"n": 1.0}) {
		t.Errorf("vendor properties not stored: %v", l)
	}
	if _, has := e.getList(other)[project]; has {
		t.Error("unset vendor property should be absent")
	}

	got := e.call("TaskList/get", obj{"ids": []string{id, other}, "properties": []string{project}})["list"].([]any)
	if !reflect.DeepEqual(got[0], obj{"id": id, project: "github.com/rjbs/Loose-Threads"}) ||
		!reflect.DeepEqual(got[1], obj{"id": other}) {
		t.Errorf("properties filter: %v", got)
	}

	state := e.call("TaskList/get", obj{"ids": []string{}})["state"].(string)
	e.call("TaskList/set", obj{"update": obj{id: obj{project: "github.com/rjbs/LooseThreads", meta + "/n": 2.0}}})
	checkChanges(t, e, "TaskList", state, nil, []string{id}, nil)
	l = e.getList(id)
	if l[project] != "github.com/rjbs/LooseThreads" || !reflect.DeepEqual(l[meta], obj{"n": 2.0}) {
		t.Errorf("vendor properties not updated: %v", l)
	}

	state = e.call("TaskList/get", obj{"ids": []string{}})["state"].(string)
	e.call("TaskList/set", obj{"update": obj{id: obj{project: nil, meta: nil}}})
	checkChanges(t, e, "TaskList", state, nil, []string{id}, nil)
	l = e.getList(id)
	for _, p := range []string{project, meta} {
		if _, has := l[p]; has {
			t.Errorf("%s should have been removed: %v", p, l)
		}
	}

	e.call("TaskList/set", obj{"update": obj{other: obj{project: "github.com/rjbs/Other"}}})
	if e.getList(other)[project] != "github.com/rjbs/Other" {
		t.Error("vendor property not added by update")
	}

	resp := e.call("TaskList/set", obj{"update": obj{other: obj{"bogus": 1}}})
	if typ, props := setErrorProps(t, resp, "notUpdated", other); typ != "invalidProperties" || !slices.Equal(props, []string{"bogus"}) {
		t.Errorf("unprefixed unknown property on update: %v", resp)
	}
	if typ := e.callError("TaskList/get", obj{"properties": []string{"bogus"}}); typ != "invalidArguments" {
		t.Errorf("unknown property in TaskList/get: %s", typ)
	}
}

func TestUseDefaultAlertsAlwaysReturned(t *testing.T) {
	e := newEnv(t)
	id := e.createTask(e.defaultListID(), obj{"title": "plain"})
	if task := e.getTask(id); task["useDefaultAlerts"] != false {
		t.Errorf("useDefaultAlerts should default to false on get: %v", task)
	}
	props := e.call("Task/get", obj{"ids": []string{id}, "properties": []string{"title"}})["list"].([]any)[0].(obj)
	if _, has := props["useDefaultAlerts"]; has {
		t.Error("useDefaultAlerts should not be added when properties are requested without it")
	}
}

// TestPrivacyConsistency covers the rule that every Task sharing a uid must
// agree on "privacy", judged after the whole Task/set has been applied.
func TestPrivacyConsistency(t *testing.T) {
	e := newEnv(t)
	listID := e.defaultListID()
	inst := func(rid, privacy string) obj {
		o := obj{"taskListIds": obj{listID: true}, "uid": "series", "title": "instance " + rid,
			"start": "2026-10-" + rid + "T09:00:00", "recurrenceId": "2026-10-" + rid + "T09:00:00"}
		if privacy != "" {
			o["privacy"] = privacy
		}
		return o
	}

	resp := e.call("Task/set", obj{"create": obj{
		"a": inst("01", "public"), "b": inst("02", "private"), "c": obj{"taskListIds": obj{listID: true}, "title": "unrelated"},
	}})
	for _, cid := range []string{"a", "b"} {
		if typ, props := setErrorProps(t, resp, "notCreated", cid); typ != "invalidProperties" || !slices.Equal(props, []string{"privacy"}) {
			t.Errorf("conflicting create %s should fail on privacy: %v", cid, resp)
		}
	}
	if resp["created"].(obj)["c"] == nil {
		t.Errorf("unrelated create should succeed: %v", resp)
	}
	checkQuery(t, e, obj{"uid": "series"}, nil)

	resp = e.call("Task/set", obj{"create": obj{"a": inst("01", ""), "b": inst("02", "public")}})
	created := resp["created"].(obj)
	if created["a"] == nil || created["b"] == nil {
		t.Fatalf("consistent creates should succeed: %v", resp)
	}
	a := created["a"].(obj)["id"].(string)
	b := created["b"].(obj)["id"].(string)

	resp = e.call("Task/set", obj{"update": obj{a: obj{"privacy": "private", "title": "changed"}}})
	if typ, _ := setErrorProps(t, resp, "notUpdated", a); typ != "invalidProperties" {
		t.Errorf("lone privacy change should fail: %v", resp)
	}
	if e.getTask(a)["title"] != "instance 01" {
		t.Error("failed update should have been unwound entirely")
	}

	resp = e.call("Task/set", obj{"update": obj{a: obj{"privacy": "private"}, b: obj{"privacy": "private"}}})
	if resp["notUpdated"] != nil {
		t.Errorf("updating every instance together should succeed: %v", resp)
	}
	if e.getTask(a)["privacy"] != "private" || e.getTask(b)["privacy"] != "private" {
		t.Error("privacy not applied")
	}
}

// TestResultReferences is the "fetching initial data" example from the
// spec: a Task/query whose ids feed a Task/get through a result reference.
func TestResultReferences(t *testing.T) {
	e := newEnv(t)
	listID := e.defaultListID()
	e.createTask(listID, obj{"title": "open", "due": "2026-10-05T09:00:00"})
	e.createTask(listID, obj{"title": "done", "progress": "completed"})

	resps := e.request(
		jmap.Invocation{Name: "Task/query", CallID: "1", Args: withAccount(obj{
			"filter": obj{"progress": []string{"needs-action", "in-process"}},
			"sort":   []obj{{"property": "due"}},
		})},
		jmap.Invocation{Name: "Task/get", CallID: "2", Args: withAccount(obj{
			"#ids": obj{"resultOf": "1", "name": "Task/query", "path": "/ids"},
		})},
		jmap.Invocation{Name: "Task/get", CallID: "3", Args: withAccount(obj{
			"#ids": obj{"resultOf": "1", "name": "Task/query", "path": "/nope"},
		})},
		jmap.Invocation{Name: "Task/get", CallID: "4", Args: withAccount(obj{
			"#ids": obj{"resultOf": "9", "name": "Task/query", "path": "/ids"},
		})},
		jmap.Invocation{Name: "Task/get", CallID: "5", Args: withAccount(obj{
			"#ids": obj{"resultOf": "2", "name": "Task/get", "path": "/list/*/id"},
		})},
	)
	if resps[1].Name != "Task/get" {
		t.Fatalf("result reference failed: %v", resps[1].Args)
	}
	list := resps[1].Args["list"].([]any)
	if len(list) != 1 || list[0].(obj)["title"] != "open" {
		t.Errorf("Task/get via #ids: %v", list)
	}
	for _, r := range resps[2:4] {
		if r.Name != "error" || r.Args["type"] != "invalidResultReference" {
			t.Errorf("call %s should fail with invalidResultReference: %v", r.CallID, r.Args)
		}
	}
	if resps[4].Name != "Task/get" || len(resps[4].Args["list"].([]any)) != 1 {
		t.Errorf("wildcard pointer reference: %v", resps[4].Args)
	}
}

// readEvent reads one SSE event from the body and returns its event name
// and decoded data.
func readEvent(t *testing.T, body interface{ Read([]byte) (int, error) }) (string, obj) {
	t.Helper()
	buf := make([]byte, 4096)
	n, err := body.Read(buf)
	if err != nil {
		t.Fatalf("reading event: %v", err)
	}
	var name string
	var data obj
	for _, line := range bytes.Split(buf[:n], []byte("\n")) {
		if rest, ok := bytes.CutPrefix(line, []byte("event: ")); ok {
			name = string(rest)
		}
		if rest, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
			json.Unmarshal(rest, &data)
		}
	}
	return name, data
}

func TestEventSourceInitialState(t *testing.T) {
	e := newEnv(t)
	taskState := e.call("Task/get", obj{"ids": []string{}})["state"]

	resp, err := http.Get(e.ts.URL + "/jmap/eventsource?types=*&closeafter=no&ping=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	name, data := readEvent(t, resp.Body)
	changed, _ := data["changed"].(obj)
	acct, _ := changed[account].(obj)
	if name != "state" || acct["Task"] != taskState || acct["TaskList"] == nil || acct["TaskNotification"] != "0" {
		t.Errorf("initial event: %s %v", name, data)
	}

	// A closeafter=state client must not be closed by the opening event;
	// with pinging on it gets a ping as its first byte instead.
	resp2, err := http.Get(e.ts.URL + "/jmap/eventsource?types=*&closeafter=state&ping=5")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if name, data := readEvent(t, resp2.Body); name != "ping" || data["interval"] != 5.0 {
		t.Errorf("closeafter=state first event: %s %v", name, data)
	}
}

func TestDemoPage(t *testing.T) {
	checkRoot := func(name string, page http.Handler, wantStatus int) {
		t.Helper()
		e := newEnvWith(t, page)
		for _, path := range []string{"/", "/index.html"} {
			resp, err := http.Get(e.ts.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != wantStatus {
				t.Errorf("%s: GET %s: status %d, want %d", name, path, resp.StatusCode, wantStatus)
			}
			if wantStatus == 200 && !bytes.Contains(body, []byte("TaskBlaster")) {
				t.Errorf("%s: GET %s: body does not look like the page", name, path)
			}
		}
	}
	checkRoot("with page", taskblaster.Handler(), 200)
	checkRoot("without page", nil, 404)
}
