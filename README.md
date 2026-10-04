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
| `-tz`       | `America/New_York` | server-side fallback zone for tasks with no `timeZone` (see below) |
| `-base-url` | derived per request| public base URL for the Session object's URLs     |

The Session object is at `/.well-known/jmap`, the API at `/jmap`, and the
push event source at `/jmap/eventsource`.  On first start, a default
TaskList named "Tasks" is created.

There is no authentication yet.  Bind to localhost or put it behind
something that provides it.

A one-page demo client, TaskBlaster, lives in [`demo-client/`](demo-client/)
along with a small proxy that serves it in front of the server.

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

## Notes

* Task/query resolves tasks that have no `timeZone` in the zone given by
  the request's `timeZone` argument (default `Etc/UTC`), for both the
  `before`/`after` filters and sorting on `due` or `start`, as the draft
  specifies.  Clients should pass the user's current zone.  The `-tz`
  flag is used only where no client is asking, currently just the
  `recurrenceOverridesBefore`/`After` window in Task/get.
* Every Task sharing a `uid` must agree on `privacy`.  A Task/set that
  would leave them disagreeing has the offending creates and updates
  rejected, after the whole request has been applied, so a client can
  change all instances in one request.
* Creating a Task whose `uid` (and `recurrenceId`) is already in use fails
  with an `alreadyExists` SetError carrying `existingId`, the shape RFC
  8620 defines for `/copy`.  A retried create whose first response was
  lost can recover its task from that.  The draft does not yet say this.
* Text search uses SQLite FTS5 with the Porter stemmer.
* The database schema is versioned with `PRAGMA user_version` and migrated
  on startup.
