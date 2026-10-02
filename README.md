# TaskMaster

A small, single-user JMAP for Task Lists server, backed by SQLite.  It
implements the data model in `draft-signes-jmap-tasklists`, trimmed to what
one person with one account needs.

## Running

    go build ./cmd/taskmaster
    ./taskmaster -listen 127.0.0.1:8080 -db taskmaster.sqlite

Flags:

| flag        | default            | meaning                                           |
|-------------|--------------------|---------------------------------------------------|
| `-listen`   | `127.0.0.1:8080`   | address to listen on                              |
| `-db`       | `taskmaster.sqlite`| SQLite database path, created on first run        |
| `-account`  | `rjbs`             | the account id and username served                |
| `-tz`       | `America/New_York` | zone used to resolve tasks that have no `timeZone`|
| `-base-url` | derived per request| public base URL for the Session object's URLs     |

The Session object is at `/.well-known/jmap`, the API at `/jmap`, and the
push event source at `/jmap/eventsource`.  On first start, a default
TaskList named "Tasks" is created.

There is no authentication yet.  Bind to localhost or put it behind
something that provides it.

## What is implemented

* `TaskList/get`, `/changes`, `/set` (including `onDestroyRemoveTasks` and
  `onSuccessSetIsDefault`)
* `Task/get`, `/changes`, `/set`, `/query` with every filter condition and
  sort property in the spec
* `TaskNotification/*`, always empty: there are no other users to be
  notified about
* Server-Sent Events push via the event source; no `PushSubscription`.
  On connect the server immediately sends a `state` event carrying the
  current states (or, for `closeafter=state`, a `ping` if pinging is on),
  so a client learns at once whether it is behind and browsers that wait
  for the first byte fire `open` promptly.
* Result references (`#ids` and friends, RFC 8620 §3.7) and creation
  references (`#creationId`) wherever an id is accepted

## What is deliberately not implemented

* Recurrence expansion.  `recurrenceRule` and `recurrenceOverrides` are
  stored and returned, but `Task/query` with `expandRecurrences` returns
  `cannotCalculateOccurrences`.
* Attachments and blob upload/download.
* Alert triggering.  Alerts are stored but never fire.
* `Task/parse`, `Task/copy`, and `queryChanges` (which returns
  `cannotCalculateChanges`).
* Sharing, Principals, and per-user properties.

## Notes on interpretation

* JSCalendar bis §4.2 says a task needs `due` or `start` when `timeZone` is
  *not* set, contradicting its own changelog.  This server follows the
  evident intent: a `timeZone` or `showWithoutTime: true` requires a date;
  a plain undated task is fine.
* Sorting on `progress` uses the order needs-action, in-process, completed,
  failed, cancelled, then anything else.  The spec does not define one.
* Text search uses SQLite FTS5 with the Porter stemmer.
