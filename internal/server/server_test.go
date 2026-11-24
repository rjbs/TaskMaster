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

	"github.com/rjbs/gizmo/auth"
	"github.com/rjbs/gizmo/jmap"
	"github.com/rjbs/gizmo/jmaptest"
	"github.com/rjbs/gizmo/push"
	"github.com/rjbs/taskmaster/internal/store"
	"github.com/rjbs/taskmaster/internal/tasks"

	taskblaster "github.com/rjbs/taskmaster/demo-client"
)

const account = "rjbs"

type obj = map[string]any

// env is a running server with an in-memory database, and a client of it
// acting in the account.
type env struct {
	*jmaptest.Tester
	t *testing.T
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
	broker := push.NewBroker()
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

	tester := jmaptest.New(t, ts.URL, jmap.CoreCapability, tasks.Capability)
	tester.AccountID = account
	return &env{Tester: tester, t: t}
}

// call runs one method and fails the test unless it is answered by a single
// sentence of the same name, which it returns.
func (e *env) call(name string, args obj) *jmaptest.Sentence {
	e.t.Helper()
	return e.Request(jmaptest.Do(name, args)).SingleSentence(name)
}

// callError runs one method and fails the test unless it produces an error
// of type want.
func (e *env) callError(name string, args obj, want string) {
	e.t.Helper()
	e.Request(jmaptest.Do(name, args)).SingleSentence("error").AssertError(want)
}

func (e *env) createList(name string, extra obj) string {
	e.t.Helper()
	props := obj{"name": name}
	for k, v := range extra {
		props[k] = v
	}
	return e.call("TaskList/set", obj{"create": obj{"new": props}}).AsSet().CreatedID("new")
}

func (e *env) createTask(listID string, props obj) string {
	e.t.Helper()
	full := obj{"taskListIds": obj{listID: true}}
	for k, v := range props {
		full[k] = v
	}
	return e.call("Task/set", obj{"create": obj{"new": full}}).AsSet().CreatedID("new")
}

func (e *env) getTask(id string) obj {
	e.t.Helper()
	list := e.call("Task/get", obj{"ids": []string{id}}).Args["list"].([]any)
	if len(list) != 1 {
		e.t.Fatalf("Task/get %s: not found", id)
	}
	return list[0].(obj)
}

func (e *env) getList(id string) obj {
	e.t.Helper()
	list := e.call("TaskList/get", obj{"ids": []string{id}}).Args["list"].([]any)
	if len(list) != 1 {
		e.t.Fatalf("TaskList/get %s: not found", id)
	}
	return list[0].(obj)
}

func (e *env) defaultListID() string {
	e.t.Helper()
	for _, l := range e.call("TaskList/get", obj{}).Args["list"].([]any) {
		if l.(obj)["isDefault"] == true {
			return l.(obj)["id"].(string)
		}
	}
	e.t.Fatal("no default task list")
	return ""
}

// checkSetError asserts that a SetError, as found by a Set's NotCreated,
// NotUpdated or NotDestroyed, has type wantType and, if wantProps are given,
// that its "properties" are exactly those.
func checkSetError(t *testing.T, se obj, wantType string, wantProps ...string) {
	t.Helper()
	var props []string
	if list, ok := se["properties"].([]any); ok {
		for _, p := range list {
			props = append(props, p.(string))
		}
	}
	sort.Strings(props)
	sort.Strings(wantProps)
	if se["type"] != wantType || (wantProps != nil && !slices.Equal(props, wantProps)) {
		t.Errorf("SetError %v: want %s %v", se, wantType, wantProps)
	}
}

func TestSession(t *testing.T) {
	e := newEnv(t)
	sess := e.Session()
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
	if sess["apiUrl"] != e.BaseURL+"/jmap/api" {
		t.Errorf("apiUrl = %v", sess["apiUrl"])
	}
}

func TestDefaultTaskListSeeded(t *testing.T) {
	e := newEnv(t)
	lists := e.call("TaskList/get", obj{}).Args["list"].([]any)
	if len(lists) != 1 || lists[0].(obj)["name"] != "Tasks" || lists[0].(obj)["isDefault"] != true {
		t.Errorf("expected one default list named Tasks, got %v", lists)
	}
}

func TestTaskListLifecycle(t *testing.T) {
	e := newEnv(t)
	id := e.createList("Groceries", obj{"color": "Teal", "sortOrder": 3})

	l := e.getList(id)
	if l["name"] != "Groceries" || l["color"] != "Teal" || l["sortOrder"] != json.Number("3") || l["isSubscribed"] != true || l["isDefault"] != false {
		t.Errorf("unexpected list after create: %v", l)
	}

	e.call("TaskList/set", obj{"update": obj{id: obj{"name": "Food", "color": nil, "sortOrder": nil}}}).AsSet().AssertNoErrors()
	l = e.getList(id)
	if l["name"] != "Food" || l["color"] != nil || l["sortOrder"] != json.Number("0") {
		t.Errorf("unexpected list after update: %v", l)
	}

	set := e.call("TaskList/set", obj{"destroy": []string{id}}).AsSet()
	if !slices.Equal(set.DestroyedIDs(), []string{id}) {
		t.Errorf("destroy failed: %v", set)
	}
	got := e.call("TaskList/get", obj{"ids": []string{id}})
	if !slices.Equal(got.Strs("notFound"), []string{id}) {
		t.Errorf("destroyed list still found: %v", got)
	}
}

func TestTaskListSetDefault(t *testing.T) {
	e := newEnv(t)
	oldDefault := e.defaultListID()

	set := e.call("TaskList/set", obj{
		"create":                obj{"w": obj{"name": "Work"}},
		"onSuccessSetIsDefault": "#w",
	}).AsSet()
	created := set.Created("w")
	if created["isDefault"] != true {
		t.Errorf("created list should report isDefault true: %v", created)
	}
	if upd := set.Updated(oldDefault); !reflect.DeepEqual(upd, obj{"isDefault": false}) {
		t.Errorf("old default should be reported updated with isDefault false: %v", upd)
	}
	if e.defaultListID() != created["id"] {
		t.Error("default did not move")
	}

	set = e.call("TaskList/set", obj{"update": obj{oldDefault: obj{"isDefault": true}}}).AsSet()
	checkSetError(t, set.NotUpdated(oldDefault), "invalidProperties", "isDefault")
}

func TestTaskListDestroyWithTasks(t *testing.T) {
	e := newEnv(t)
	keep := e.createList("Keep", nil)
	doomed := e.createList("Doomed", nil)
	only := e.createTask(doomed, obj{"title": "only in doomed"})
	both := e.createTask(doomed, obj{"title": "in both", "taskListIds": obj{doomed: true, keep: true}})

	set := e.call("TaskList/set", obj{"destroy": []string{doomed}}).AsSet()
	checkSetError(t, set.NotDestroyed(doomed), tasks.SetErrTaskListHasTask)

	set = e.call("TaskList/set", obj{"destroy": []string{doomed}, "onDestroyRemoveTasks": true}).AsSet()
	if !slices.Equal(set.DestroyedIDs(), []string{doomed}) {
		t.Fatalf("destroy failed: %v", set)
	}
	got := e.call("Task/get", obj{"ids": []string{only, both}})
	if !slices.Equal(got.Strs("notFound"), []string{only}) {
		t.Errorf("task only in the destroyed list should be gone: %v", got)
	}
	list := got.Args["list"].([]any)
	if len(list) != 1 || !reflect.DeepEqual(list[0].(obj)["taskListIds"], obj{keep: true}) {
		t.Errorf("task in both lists should remain only in Keep: %v", list)
	}
}

// checkListInvalid asserts that creating a list with props fails with an
// invalidProperties SetError naming exactly wantProps.
func checkListInvalid(t *testing.T, e *env, props obj, wantProps ...string) {
	t.Helper()
	set := e.call("TaskList/set", obj{"create": obj{"x": props}}).AsSet()
	checkSetError(t, set.NotCreated("x"), "invalidProperties", wantProps...)
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
	s := e.Request(
		jmaptest.Do("TaskList/set", obj{
			"create": obj{"l": obj{"name": "Errands"}},
		}),
		jmaptest.Do("Task/set", obj{
			"create": obj{"k1": obj{
				"taskListIds": obj{"#l": true},
				"title":       "Renew passport",
				"due":         "2026-09-01T09:00:00",
				"timeZone":    "Australia/Melbourne",
				"priority":    3,
				"keywords":    obj{"travel": true},
			}},
		}),
		jmaptest.Do("Task/get", obj{
			"ids": []string{"#k1"},
		}),
	).AssertSentences(3)
	listID := s[0].AssertNamed("TaskList/set").AsSet().CreatedID("l")
	created := s[1].AssertNamed("Task/set").AsSet().Created("k1")
	for _, p := range []string{"id", "version", "uid", "created", "updated"} {
		if created[p] == nil {
			t.Errorf("created response lacks %s: %v", p, created)
		}
	}
	taskID := created["id"].(string)

	fetched := s[2].AssertNamed("Task/get").Args["list"].([]any)[0].(obj)
	if fetched["title"] != "Renew passport" || fetched["@type"] != "Task" || fetched["version"] != "2.0" || fetched["priority"] != json.Number("3") {
		t.Errorf("unexpected task: %v", fetched)
	}
	if !reflect.DeepEqual(fetched["taskListIds"], obj{listID: true}) {
		t.Errorf("taskListIds = %v", fetched["taskListIds"])
	}

	set := e.call("Task/set", obj{"update": obj{taskID: obj{
		"progress":        "completed",
		"percentComplete": 100,
		"keywords/urgent": true,
		"priority":        nil,
	}}}).AsSet()
	if !slices.Equal(set.UpdatedIDs(), []string{taskID}) {
		t.Fatalf("update failed: %v", set)
	}
	task := e.getTask(taskID)
	if task["progress"] != "completed" || task["percentComplete"] != json.Number("100") {
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

	set = e.call("Task/set", obj{"update": obj{taskID: obj{"id": "T-other"}}}).AsSet()
	checkSetError(t, set.NotUpdated(taskID), "invalidProperties")

	props := e.call("Task/get", obj{"ids": []string{taskID}, "properties": []string{"title"}}).Args["list"].([]any)[0].(obj)
	if len(props) != 2 || props["id"] != taskID || props["title"] != "Renew passport" {
		t.Errorf("properties filter: %v", props)
	}

	set = e.call("Task/set", obj{"destroy": []string{taskID}}).AsSet()
	if !slices.Equal(set.DestroyedIDs(), []string{taskID}) {
		t.Errorf("destroy failed: %v", set)
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
	set := e.call("Task/set", obj{"create": obj{"x": full}}).AsSet()
	checkSetError(t, set.NotCreated("x"), "invalidProperties", wantProps...)
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
	set := e.call("Task/set", obj{"create": obj{"x": obj{"taskListIds": obj{listID: true}, "uid": "dup", "title": "second"}}}).AsSet()
	if se := set.NotCreated("x"); se["type"] != "alreadyExists" || se["existingId"] != first {
		t.Errorf("duplicate uid on create: %v", se)
	}
	other := e.createTask(listID, obj{"uid": "other", "title": "third"})
	set = e.call("Task/set", obj{"update": obj{other: obj{"uid": "dup"}}}).AsSet()
	checkSetError(t, set.NotUpdated(other), "invalidProperties", "uid")
	set = e.call("Task/set", obj{"update": obj{other: obj{"version": nil}}}).AsSet()
	checkSetError(t, set.NotUpdated(other), "invalidProperties", "version")

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
	set := e.call("Task/set", obj{"update": obj{id: obj{"due": nil}}}).AsSet()
	checkSetError(t, set.NotUpdated(id), "invalidProperties", "alerts")
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
	titles := []string{}
	for _, id := range e.call("Task/query", args).Strs("ids") {
		titles = append(titles, e.getTask(id)["title"].(string))
	}
	if wantTitles == nil {
		wantTitles = []string{}
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
	ids := e.call("Task/query", obj{"sort": []obj{{"property": "id", "isAscending": ascending}}}).Strs("ids")
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
	if resp.Int("total") != 6 || resp.Int("position") != 1 || len(resp.Strs("ids")) != 2 {
		t.Errorf("paging: %v", resp)
	}
	resp = e.call("Task/query", obj{"sort": byDue, "position": -2})
	if resp.Int("position") != 4 || len(resp.Strs("ids")) != 2 {
		t.Errorf("negative position: %v", resp)
	}

	e.callError("Task/query", obj{"sort": []obj{{"property": "title"}}}, "unsupportedSort")
	checkIDSort(t, e, true, 6)
	checkIDSort(t, e, false, 6)

	e.callError("Task/query", obj{"sort": []obj{{"property": "recurrenceId"}}}, "unsupportedSort")
	e.callError("Task/query", obj{"filter": obj{"color": "red"}}, "unsupportedFilter")
	e.callError("Task/query", obj{"expandRecurrences": true, "filter": obj{"after": "2026-01-01T00:00:00", "before": "2026-02-01T00:00:00"}},
		tasks.ErrCannotCalculateOccurrences)
}

func TestChanges(t *testing.T) {
	e := newEnv(t)
	listID := e.defaultListID()
	state0 := e.call("Task/get", obj{"ids": []string{}}).Str("state")

	a := e.createTask(listID, obj{"title": "a"})
	b := e.createTask(listID, obj{"title": "b"})
	state1 := e.call("Task/get", obj{"ids": []string{}}).Str("state")

	e.call("Task/set", obj{"update": obj{a: obj{"title": "a2"}}}).AsSet().AssertNoErrors()
	e.call("Task/set", obj{"destroy": []string{b}}).AsSet().AssertNoErrors()
	c := e.createTask(listID, obj{"title": "c"})
	e.call("Task/set", obj{"destroy": []string{c}}).AsSet().AssertNoErrors()

	checkChanges(t, e, "Task", state0, []string{a}, nil, nil)
	checkChanges(t, e, "Task", state1, nil, []string{a}, []string{b})

	resp := e.call("Task/changes", obj{"sinceState": state0, "maxChanges": 1})
	if resp.Args["hasMoreChanges"] != true {
		t.Errorf("expected hasMoreChanges with maxChanges 1: %v", resp)
	}
	resp = e.call("Task/changes", obj{"sinceState": resp.Str("newState")})
	if resp.Args["hasMoreChanges"] != false {
		t.Errorf("second page should finish: %v", resp)
	}

	e.callError("Task/changes", obj{"sinceState": "999999"}, "cannotCalculateChanges")
	e.callError("Task/set", obj{"ifInState": state0, "destroy": []string{a}}, "stateMismatch")
	e.callError("Task/queryChanges", obj{"sinceQueryState": state0}, "cannotCalculateChanges")
}

// checkChanges compares a /changes response against the expected id sets,
// ignoring order within each set.
func checkChanges(t *testing.T, e *env, typ, since string, created, updated, destroyed []string) {
	t.Helper()
	resp := e.call(typ+"/changes", obj{"sinceState": since})
	for name, want := range map[string][]string{"created": created, "updated": updated, "destroyed": destroyed} {
		got := resp.Strs(name)
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
	if len(resp.Args["list"].([]any)) != 0 || !slices.Equal(resp.Strs("notFound"), []string{"n1"}) {
		t.Errorf("get: %v", resp)
	}
	resp = e.call("TaskNotification/query", obj{})
	if len(resp.Strs("ids")) != 0 {
		t.Errorf("query: %v", resp)
	}
	set := e.call("TaskNotification/set", obj{"create": obj{"x": obj{}}}).AsSet()
	checkSetError(t, set.NotCreated("x"), "forbidden")
}

func TestRequestErrors(t *testing.T) {
	e := newEnv(t)
	e.Post(`{not json`).AssertRequestError(t, jmap.ReqErrNotJSON)
	e.Post(`{"using": []}`).AssertRequestError(t, jmap.ReqErrNotRequest)
	e.Post(`{"using": ["urn:example:nope"], "methodCalls": []}`).AssertRequestError(t, jmap.ReqErrUnknownCapability)
	// RFC 8620 does not oblige a request to use the core capability.
	e.Post(fmt.Sprintf(`{"using": [%q], "methodCalls": []}`, tasks.Capability)).Response(t)

	e.callError("Task/get", obj{"accountId": "nobody"}, "accountNotFound")
	e.callError("Task/copy", obj{}, "unknownMethod")
}

func TestEventSource(t *testing.T) {
	e := newEnv(t)
	stream := e.Listen("types=Task&closeafter=state")
	if stream.Status != 200 {
		t.Fatalf("event source: status %d", stream.Status)
	}
	if ct := stream.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}

	// Give the subscriber a moment to register before we change anything.
	time.Sleep(50 * time.Millisecond)
	e.createList("Only lists changed", nil)
	e.createTask(e.defaultListID(), obj{"title": "poke"})

	acct := stream.NextState()[account]
	if acct["Task"] == "" || acct["TaskList"] != "" {
		t.Errorf("unexpected event: %v", acct)
	}
	stream.AssertClosed()
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
	e.call("Task/set", obj{"update": obj{id: obj{
		"progress":        "cancelled",
		"progressUpdated": "2026-09-14T12:00:07Z",
		endedAt:           "2026-09-13",
	}}}).AsSet().AssertNoErrors()
	task = e.getTask(id)
	if task["progress"] != "cancelled" || task[endedAt] != "2026-09-13" {
		t.Errorf("sweep not applied: %v", task)
	}

	// Reopening, plus a category rename and a link change in the same patch.
	e.call("Task/set", obj{"update": obj{id: obj{
		"progress":                           "needs-action",
		"progressUpdated":                    nil,
		endedAt:                              nil,
		category:                             nil,
		"keywords/nerfjournal:category:Home": true,
		"links/nerfjournal-external/href":    "https://example.com/pr/43",
	}}}).AsSet().AssertNoErrors()
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
	e.call("Task/set", obj{"update": obj{id: obj{"progress": "cancelled"}}}).AsSet().AssertNoErrors()
	checkQuery(t, e, obj{"progress": []string{"cancelled"}}, nil, "Review the release notes")
	props := e.call("Task/get", obj{"ids": []string{id}, "properties": []string{autoCancel}}).Args["list"].([]any)[0].(obj)
	if len(props) != 2 || props[autoCancel] != true {
		t.Errorf("vendor property filter: %v", props)
	}

	// RFC 8620 forbids patching below a parent that does not exist, so a
	// task with no keywords yet must be given the whole map.
	bare := e.createTask(e.defaultListID(), obj{"title": "foreign task"})
	set := e.call("Task/set", obj{"update": obj{bare: obj{category: true}}}).AsSet()
	checkSetError(t, set.NotUpdated(bare), "invalidPatch")
	e.call("Task/set", obj{"update": obj{bare: obj{"keywords": obj{"nerfjournal:category:Work": true}}}}).AsSet().AssertNoErrors()

	checkTaskInvalid(t, e, obj{"progressUpdated": "2026-09-13"}, "progressUpdated")
}

// TestTaskListVendorProperties covers the vendor-prefixed TaskList
// properties Loose Threads uses to keep a project's identity on its list.
func TestTaskListVendorProperties(t *testing.T) {
	e := newEnv(t)
	const project = "lt.semiotic.systems:project"
	const meta = "lt.semiotic.systems:meta"

	id := e.createList("Loose Threads", obj{project: "github.com/rjbs/Loose-Threads", meta: obj{"n": 1}})
	other := e.createList("Other", nil)
	l := e.getList(id)
	if l[project] != "github.com/rjbs/Loose-Threads" || !reflect.DeepEqual(l[meta], obj{"n": json.Number("1")}) {
		t.Errorf("vendor properties not stored: %v", l)
	}
	if _, has := e.getList(other)[project]; has {
		t.Error("unset vendor property should be absent")
	}

	got := e.call("TaskList/get", obj{"ids": []string{id, other}, "properties": []string{project}}).Args["list"].([]any)
	if !reflect.DeepEqual(got[0], obj{"id": id, project: "github.com/rjbs/Loose-Threads"}) ||
		!reflect.DeepEqual(got[1], obj{"id": other}) {
		t.Errorf("properties filter: %v", got)
	}

	state := e.call("TaskList/get", obj{"ids": []string{}}).Str("state")
	e.call("TaskList/set", obj{"update": obj{id: obj{project: "github.com/rjbs/LooseThreads", meta + "/n": 2}}}).AsSet().AssertNoErrors()
	checkChanges(t, e, "TaskList", state, nil, []string{id}, nil)
	l = e.getList(id)
	if l[project] != "github.com/rjbs/LooseThreads" || !reflect.DeepEqual(l[meta], obj{"n": json.Number("2")}) {
		t.Errorf("vendor properties not updated: %v", l)
	}

	state = e.call("TaskList/get", obj{"ids": []string{}}).Str("state")
	e.call("TaskList/set", obj{"update": obj{id: obj{project: nil, meta: nil}}}).AsSet().AssertNoErrors()
	checkChanges(t, e, "TaskList", state, nil, []string{id}, nil)
	l = e.getList(id)
	for _, p := range []string{project, meta} {
		if _, has := l[p]; has {
			t.Errorf("%s should have been removed: %v", p, l)
		}
	}

	e.call("TaskList/set", obj{"update": obj{other: obj{project: "github.com/rjbs/Other"}}}).AsSet().AssertNoErrors()
	if e.getList(other)[project] != "github.com/rjbs/Other" {
		t.Error("vendor property not added by update")
	}

	set := e.call("TaskList/set", obj{"update": obj{other: obj{"bogus": 1}}}).AsSet()
	checkSetError(t, set.NotUpdated(other), "invalidProperties", "bogus")
	e.callError("TaskList/get", obj{"properties": []string{"bogus"}}, "invalidArguments")
}

func TestUseDefaultAlertsAlwaysReturned(t *testing.T) {
	e := newEnv(t)
	id := e.createTask(e.defaultListID(), obj{"title": "plain"})
	if task := e.getTask(id); task["useDefaultAlerts"] != false {
		t.Errorf("useDefaultAlerts should default to false on get: %v", task)
	}
	props := e.call("Task/get", obj{"ids": []string{id}, "properties": []string{"title"}}).Args["list"].([]any)[0].(obj)
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

	set := e.call("Task/set", obj{"create": obj{
		"a": inst("01", "public"), "b": inst("02", "private"), "c": obj{"taskListIds": obj{listID: true}, "title": "unrelated"},
	}}).AsSet()
	for _, cid := range []string{"a", "b"} {
		checkSetError(t, set.NotCreated(cid), "invalidProperties", "privacy")
	}
	set.Created("c")
	checkQuery(t, e, obj{"uid": "series"}, nil)

	set = e.call("Task/set", obj{"create": obj{"a": inst("01", ""), "b": inst("02", "public")}}).AsSet()
	a := set.CreatedID("a")
	b := set.CreatedID("b")

	set = e.call("Task/set", obj{"update": obj{a: obj{"privacy": "private", "title": "changed"}}}).AsSet()
	checkSetError(t, set.NotUpdated(a), "invalidProperties")
	if e.getTask(a)["title"] != "instance 01" {
		t.Error("failed update should have been unwound entirely")
	}

	e.call("Task/set", obj{"update": obj{a: obj{"privacy": "private"}, b: obj{"privacy": "private"}}}).AsSet().AssertNoErrors()
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

	s := e.Request(
		jmaptest.Do("Task/query", obj{
			"filter": obj{"progress": []string{"needs-action", "in-process"}},
			"sort":   []obj{{"property": "due"}},
		}).As("1"),
		jmaptest.Do("Task/get", obj{"#ids": jmaptest.Ref("1", "Task/query", "/ids")}).As("2"),
		jmaptest.Do("Task/get", obj{"#ids": jmaptest.Ref("1", "Task/query", "/nope")}).As("3"),
		jmaptest.Do("Task/get", obj{"#ids": jmaptest.Ref("9", "Task/query", "/ids")}).As("4"),
		jmaptest.Do("Task/get", obj{"#ids": jmaptest.Ref("2", "Task/get", "/list/*/id")}).As("5"),
	).AssertSentences(5)
	list := s[1].AssertNamed("Task/get").Args["list"].([]any)
	if len(list) != 1 || list[0].(obj)["title"] != "open" {
		t.Errorf("Task/get via #ids: %v", list)
	}
	s[2].AssertError("invalidResultReference")
	s[3].AssertError("invalidResultReference")
	if len(s[4].AssertNamed("Task/get").Args["list"].([]any)) != 1 {
		t.Errorf("wildcard pointer reference: %v", s[4])
	}
}

func TestEventSourceInitialState(t *testing.T) {
	e := newEnv(t)
	taskState := e.call("Task/get", obj{"ids": []string{}}).Str("state")

	acct := e.Listen("types=*&closeafter=no&ping=0").NextState()[account]
	if acct["Task"] != taskState || acct["TaskList"] == "" || acct["TaskNotification"] != "0" {
		t.Errorf("initial event: %v", acct)
	}

	// A closeafter=state client must not be closed by the opening event;
	// with pinging on it gets a ping as its first byte instead.
	ev := e.Listen("types=*&closeafter=state&ping=5").Next()
	var data struct{ Interval int }
	ev.Decode(t, &data)
	if ev.Type != "ping" || data.Interval != 5 {
		t.Errorf("closeafter=state first event: %s %s", ev.Type, ev.Data)
	}
}

func TestDemoPage(t *testing.T) {
	checkRoot := func(name string, page http.Handler, wantStatus int) {
		t.Helper()
		e := newEnvWith(t, page)
		for _, path := range []string{"/", "/index.html"} {
			resp, err := http.Get(e.BaseURL + path)
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
