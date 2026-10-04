# TaskBlaster

A one-page demo client for a [JMAP for Task Lists][draft] server, built to
show off [TaskMaster](..), the server this repository is mostly about.  Plain HTML and JavaScript, no build
step, no framework, and only a nod to Mondrian.

[draft]: https://github.com/rjbs/jmap-for-tasks

## Running

TaskMaster serves JMAP and nothing else, so the page can't be loaded from it
directly, and it doesn't send CORS headers, so the page can't be loaded from
anywhere else either.  The `taskblaster` script fills the gap: it serves
`index.html` and forwards `/jmap` and `/.well-known/jmap` to TaskMaster.

    ./taskblaster -listen 127.0.0.1:8081 -backend 127.0.0.1:8080

Then open <http://127.0.0.1:8081/>.

## What it demonstrates

* Bootstrapping from the Session object at `/.well-known/jmap`, using
  `primaryAccounts` to find the account and `apiUrl` for the API.
* `TaskList/get` to populate the sidebar, and `TaskList/set` to create lists
  and destroy them (with `onDestroyRemoveTasks`).
* `Task/query` with `inTaskList` and `progress` filters, sorted by `due`
  then `created`, with the client's own `timeZone` argument, chained to
  `Task/get` by a back-reference in a single request.
* `Task/set` to create tasks (date-only tasks use `showWithoutTime`),
  rename them, mark them done (`progress` *and* `percentComplete`, since
  the spec says one doesn't imply the other), and destroy them.
* Push over the event source: the page watches for `StateChange` events
  and refetches only the types whose state it hasn't seen.

## What it doesn't

Recurrence, alerts, attachments, sharing, and anything else that would make
it more than a demo.
