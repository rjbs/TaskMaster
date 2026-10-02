package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/rjbs/taskmaster/internal/jmap"
	"github.com/rjbs/taskmaster/internal/store"
	"github.com/rjbs/taskmaster/internal/tasks"
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
	proc := jmap.NewProcessor()
	svc.Register(proc)
	broker := NewBroker()
	db.OnCommit(broker.Publish)

	ts := httptest.NewServer(New(Config{AccountID: account, Tasks: svc, Processor: proc, Broker: broker}))
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
	for _, p := range []string{"id", "uid", "created", "updated"} {
		if created[p] == nil {
			t.Errorf("created response lacks %s: %v", p, created)
		}
	}
	taskID := created["id"].(string)

	fetched := resps[2].Args["list"].([]any)[0].(obj)
	if fetched["title"] != "Renew passport" || fetched["@type"] != "Task" || fetched["priority"] != 3.0 {
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

	e.createTask(listID, obj{"uid": "dup", "title": "first"})
	checkTaskInvalid(t, e, obj{"uid": "dup", "title": "second"}, "uid")
}

// checkQuery runs Task/query with filter and sort and checks the titles of
// the results, in order.
func checkQuery(t *testing.T, e *env, filter any, sortBy []obj, wantTitles ...string) {
	t.Helper()
	args := obj{"timeZone": "America/New_York"}
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
