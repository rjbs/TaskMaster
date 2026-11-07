# TaskMaster

A small JMAP for Task Lists server, backed by SQLite.  It implements the
data model in `draft-signes-jmap-tasklists`, trimmed to what one person
needs: a few accounts, each with its own database, and no sharing.

## Running

    go build ./cmd/taskmaster
    cp taskmaster.example.json taskmaster.json   # and edit
    ./taskmaster -config taskmaster.json

The Session object is at `/.well-known/jmap`, the API at `/jmap`, and the
push event source at `/jmap/eventsource`.  On first start, each account
gets a default TaskList named "Tasks".

## Users, accounts, and credentials

The configuration file (JSON; see `taskmaster.example.json` and the
`config` package documentation) describes three kinds of thing:

* a **user** is an actor who can authenticate;
* an **account** is a JMAP account, a body of data in its own SQLite file,
  owned by exactly one user, who has complete access to it.  A user may
  own several accounts.  A user with one account has it as the Session's
  primary account; a user with several has no primary account unless it
  names one with `primaryAccount`;
* a **credential** is a way a user proves who they are.  There are two
  types.  `loopback` means "requests arriving on the plain `listen`
  address", which at most one user may hold.  `tailscale` names a
  Tailscale login (like `rjbs`).  `bearer` accepts a token sent as
  `Authorization: Bearer <token>`, on either listener; see below.  A
  credential may also give
  `accountIds`, a non-empty list of the user's accounts; requests made
  with that credential see only those accounts.  If the user's primary
  account is not among them, they see no primary account.

The Session object a user sees lists only the accounts they own, and a
method call naming any other account fails with `accountNotFound`.

| setting     | default            | meaning                                           |
|-------------|--------------------|---------------------------------------------------|
| `listen`    | `127.0.0.1:8080`   | plain HTTP listener; empty disables it            |
| `baseUrl`   | derived per request| public base URL for the Session object URLs       |
| `timeZone`  | `America/New_York` | server-side fallback zone for tasks with no `timeZone` (see below) |
| `dataDir`   | the config file dir| where `<account>.sqlite` files live unless an account gives a `path` |
| `tailscale` | absent             | `{ "hostname": ..., "dir": ..., "https": ... }`; see below |
| `demoClient`| `true`             | serve the TaskBlaster page at `/` on every listener  |

Relative paths are resolved against the directory holding the config file.

### Bearer tokens

`taskmaster new-token` makes a random token and prints it, along with
the credential that accepts it.  A token looks like
`taskmaster-` followed by 49 letters and digits, the last six of which
are a checksum of the rest, so a secret scanner can recognize a real
token by itself; the server refuses a malformed one before looking it
up.  The credential is:

    { "type": "bearer", "tokenHash": "1ae16ce2...b21de2" }

Add the credential to a user, and give the token to the client.  The
configuration holds only the token's SHA-256 digest, so a copy of the
file does not grant access; a lost token cannot be recovered, only
replaced.  Add `accountIds` to the credential to limit what the token
can see.

A request with a bearer token acts as that token's user on either
listener, whatever the listener would otherwise decide; a tagged
Tailscale node can use one.  A request with an unknown token is refused
rather than treated as if it had none.  The plain listener speaks HTTP,
so a token sent to it travels unencrypted; keep it on the loopback
address, or put TLS in front of it.

### Tailscale

With a `tailscale` section, the server joins your tailnet as its own node
(via tsnet, in userspace, so no `tailscaled` is needed on the host) and
serves HTTPS on `https://<hostname>.<tailnet>.ts.net/`, with a certificate
Tailscale obtains for it.  That requires HTTPS to be enabled in the
tailnet admin console.  With `"https": false` it serves plain HTTP on
port 80 instead; the tailnet still encrypts the traffic, but iOS clients
will want the real thing.  Requests are authenticated by the connecting
node's login name; tagged nodes and logins no user holds are refused, and
the refusal is logged with the login so you can add it.

The first start must authorize the node: set `TS_AUTHKEY` to an auth key,
or watch the log for a login URL to visit.  Node state is kept in
`tailscale.dir` (default `<dataDir>/tailscale`) so later starts need
nothing.  Do not copy that state between machines of different
operating systems: the coordination server notices the OS change and
refuses the node, which shows up in the log as a health warning.  Start
with an empty state directory on the new machine and authorize it again,
after deleting the old node in the admin console so the name is free.

### Upgrading from the single-database version

Earlier builds took `-db taskmaster.sqlite -account rjbs`.  Make the
account's `path` point at that file (or move it to `<dataDir>/rjbs.sqlite`)
and keep the account id the same, since clients cache account ids.

A one-page demo client, TaskBlaster, lives in [`demo-client/`](demo-client/).
The server embeds it and serves it at `/`, so once TaskMaster is up the
client is at <http://127.0.0.1:8080/> or `https://<hostname>.<tailnet>.ts.net/`,
behind the same authentication as the JMAP endpoints.  Set
`"demoClient": false` to serve JMAP alone.

## What is implemented

* `TaskList/get`, `/changes`, `/set` (including `onDestroyRemoveTasks` and
  `onSuccessSetIsDefault`).  Vendor-prefixed properties in the RFC 8984
  form `example.com:name` are stored as given and returned; other unknown
  properties are rejected.
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
* Sharing, Principals, and per-user properties.  Each account has exactly
  one user with every right.
* Any authentication for arbitrary internet clients (passkeys, OAuth).
  The credential types are what one person on a tailnet needs.

## Notes

* Task/query resolves tasks that have no `timeZone` in the zone given by
  the request's `timeZone` argument (default `Etc/UTC`), for both the
  `before`/`after` filters and sorting on `due` or `start`, as the draft
  specifies.  Clients should pass the user's current zone.  The
  `timeZone` setting is used only where no client is asking, currently just the
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
