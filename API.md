# torrmedia HTTP API

The client's single entry point. The client never needs to know qBittorrent's
address or credentials.

Three mechanisms, each doing what it is best at:

| | |
|---|---|
| **commands** | REST: resources in the path, the verb is the HTTP method, parameters in the body |
| **state** | SSE: the server pushes a snapshot on every change; the client does not poll |
| **video** | a plain `GET` with Range, which any external player understands |

## Principles

There are five, and the rest of this document follows from them.

**1. The client does not know the internals.** No field of a response is an
infohash, a qBittorrent file index, a disk path or a state like `stoppedUP`.
All of that is an implementation detail of torrmedia. The client works with
opaque `id`s and a normalized status vocabulary.

**2. The server says what is allowed.** Every item and every file has an
`actions` field: the list of actions allowed right now. The client does not
derive this from statuses, qBittorrent availability or anything else: it
renders what is in the list and does not render what is not. An ambiguous file,
a recheck in progress, a file being deleted, qBittorrent unavailable: in every
case the server simply leaves the action out of the list. Only momentary
conflicts are left to the request itself: whether a player is reading a file
right now is not reflected in `actions`, and deleting such a file gets
`409 streaming`.

**3. One state, one source.** The client does not merge several responses into
a picture of the world. The server sends the complete picture; on a change, a
new complete one. No deltas, no sequence numbers, no reconciliation.

**4. Everything that touches qBittorrent is an operation.** Such a request
immediately returns `202` and an operation ID; progress and the result arrive in
the event stream. There are no synchronous requests that take minutes, and
client timeouts are no longer part of the protocol.

**5. The answer is the HTTP status code.** `2xx` means it worked, `4xx`/`5xx`
means it did not. There are no envelopes like `{"ok": …}`. A success body is
data, an error body is `{"error", "code"}`.

## General

**IDs** are opaque: the client does not parse them, compare them piecewise or
construct them. It only takes them from a response and puts them into a
request. They contain no characters that need escaping and are stable for as
long as the object exists; in particular, they survive adding and removing the
torrent.

**Handshake.** `GET /health` returns `contract`. The client and the server ship
together, so the client checks this value on connect and refuses to work on a
mismatch. That catches a forgotten client update at once, instead of leaving a
half-working screen.

The value is **1** (`contractVersion` in [runtime.go](runtime.go)). It is a
handshake, not an API version: it is not bumped when the API changes, because
there is no backward compatibility to track. Bumping it would describe nothing
and would only break clients that are already installed.

**Host names.** The server answers only to a `Host` (compared without the port,
case and a trailing dot) that is an IP address, `localhost` or a name under
`.localhost`, a name under `.local`, or a name listed in `TORRMEDIA_HOSTS`. Any
other name gets `421 misdirected`. This applies to every endpoint, `/health`
and streams included, and is what keeps DNS rebinding out.

**Authentication.** If `TORRMEDIA_TOKEN` is set, `Authorization: Bearer <token>`
is required on all endpoints except `/health`. The comparison is constant-time.

**Request bodies** must be sent as `Content-Type: application/json`
(parameters such as `charset` are fine): a request other than `GET` or `HEAD`
that carries a body of another type gets `415 unsupported_media_type`. Unknown
fields are rejected with `400`, and a body is limited to 1 MiB. A command
without parameters needs no body. The identity of a resource is always in the
path, never in the body. Modifiers for `DELETE` go in the query string, because
some HTTP clients do not send a body with `DELETE`.

**Browser requests.** A request other than `GET` or `HEAD` that carries
`Sec-Fetch-Site: cross-site` gets `403 refused`, with or without a body: a web
page on another site sent it through the user's browser.

The checks run in this order: host name, then cross-site and content type, then
the token. While torrmedia is shutting down, every request other than `GET` or
`HEAD` gets `503 unavailable`.

**Errors.**

```json
{ "error": "file is being streamed right now; nothing deleted", "code": "streaming" }
```

Running out of disk space and a missed deadline do not show up here: they
happen inside an operation, and their text arrives in the operation's `error`
field, not as an HTTP status.

| `code` | HTTP | Meaning |
|---|---|---|
| `bad_request` | 400, 409 | `400`: a malformed id, body or query, or an invalid value in it; `409`: the request does not apply to the object as it is (no viewed mark to clear, file not excluded, file already downloaded, draft already confirmed) |
| `unauthorized` | 401 | token missing or wrong |
| `refused` | 403 | not a `magnet:` link or one with control characters, a file outside the media root, a cross-site request |
| `not_found` | 404, 409 | `404`: no such item, file, draft or operation; `409`: the object lacks what the action needs (no torrent behind the item, file not fully downloaded, nothing watched to delete) |
| `ambiguous` | 409 | the object belongs to more than one torrent, or the magnet is already in the library |
| `busy` | 409 | an operation is already running for this item, its state does not allow the action right now (not stopped, being checked, being removed), the draft is not in a status that allows it, too many drafts are open |
| `streaming` | 409 | a player is reading the file right now |
| `unsupported_media_type` | 415 | the body is not declared as `application/json` |
| `misdirected` | 421 | the host name is not served (see `TORRMEDIA_HOSTS`) |
| `internal` | 500 | a local failure, such as saving the viewed marks |
| `qbittorrent_unavailable` | 502 | qBittorrent did not respond, or `QBIT_URL` is not set |
| `unavailable` | 503 | torrmedia is shutting down |

Responses that Go's HTTP server writes on its own are plain text, not
`{"error", "code"}`: an unknown route (`404`), a wrong method (`405`), a range
that cannot be satisfied (`416`).

---

## State

### `GET /events`

A `text/event-stream` with the list of items. The server sends a snapshot right
after connecting and then on every change (coalesced). The server itself polls
qBittorrent, once for all clients.

```text
event: state
data: {"items":[…],"degraded":null}   // degraded: string or null

event: ping
data: {}
```

`ping` is sent every 20 seconds so that intermediaries do not drop an idle
connection.

After a disconnect the client reconnects and gets a fresh complete snapshot: no
recovery, replay or reconciliation is needed.

### `GET /items/{item}/events`

The same for a single item: the snapshot includes the list of files. This
stream also carries `operation` events for the item's operations.

When the item is no longer in the catalog (it was removed with its data, or the
last file of an item with no torrent was deleted), the stream sends
`event: state` with `data: null`, and the client leaves the screen. Subscribing
to an item that does not exist is a `404`.

### `GET /items` · `GET /items/{item}`

The same bodies as a one-shot request. They are there for the first frame, as a
fallback if the stream does not come up, and so that everything can be
inspected with `curl`.

### Item

```json
{
  "id": "p:c2hvd3MvUmVsZWFzZQ",
  "title": "Some.Show.S07",
  "status": "seeding",
  "progress": 1.0,
  "size": 52430118982,
  "downloaded": 52430118982,
  "files_total": 7,
  "files_ready": 5,
  "files_watched": 3,
  "category": "Shows",
  "magnet": "magnet:?xt=urn:btih:…",
  "added_at": 1785000000,
  "transfer": { "down": 0, "up": 0, "eta": null, "ratio": 0.0055, "seeds": 1, "peers": 0 },
  "actions": ["stop", "recheck", "remove", "remove_with_data"]
}
```

`status` is torrmedia's own vocabulary, not qBittorrent's:

| | |
|---|---|
| `downloading` | downloading |
| `seeding` | seeding |
| `stopped` | stopped |
| `checking` | data check in progress, or qBittorrent is moving the files |
| `error` | qBittorrent reported an error or missing files, or two torrents share the item's folder |
| `shelf` | no torrent; the files are just lying on disk |
| `unknown` | qBittorrent is unavailable; the state cannot be determined |

`transfer.eta` is seconds, or `null` if unknown. `category` and `magnet` are
`null` when not applicable. `magnet` is present in the list as well: `id` is
opaque, so the client has nothing to build a link from, and "share" has to work
straight from the list. For a private tracker it carries the passkey.

`GET /items/{item}` additionally contains `files`.

### File

```json
{
  "id": "ZTAxLm1rdg",
  "name": "e01.mkv",
  "path": "Release/e01.mkv",
  "size": 3221225472,
  "progress": 1.0,
  "status": "ready",
  "priority": "normal",
  "watch": "watched",
  "position": 1610612736,
  "actions": ["play", "delete", "set_viewed", "clear_viewed"]
}
```

| `status` | |
|---|---|
| `ready` | fully on disk, can be played |
| `partial` | on disk, but not complete |
| `skipped` | excluded from the torrent |
| `missing` | part of the torrent, but not on disk at all |

`partial` deliberately describes completeness, not activity: it covers both a
file being downloaded right now and a leftover from a removed torrent. How
complete it is, is given by `progress`; whether it is downloading at all, by
the item's status. A file of a torrent saved outside the media root is never
`ready`: there is nothing the server could stream.

`priority` is the download priority: `"normal"`, `"high"` or `"max"`; `null` if
the file is excluded (`skipped`) or there is no torrent behind the item. "Do
not download" is not part of this vocabulary: excluding a file is `delete`,
bringing it back is `enable`.

`watch` is `null`, `"started"` or `"watched"`. `position` is the byte offset to
resume from. A mark set by hand holds until 5% of the file has been played
after it; from then on playback updates it again.

`path` is the path relative to the item, for displaying nesting only. You must
not address anything by it or derive anything from it: addressing is by `id`
only.

### Actions

| Item | File |
|---|---|
| `start`, `stop`, `recheck`, `delete_watched`, `remove`, `remove_with_data`, `clear_viewed` | `play`, `delete`, `enable`, `set_priority`, `set_viewed`, `clear_viewed` |

`delete_watched` is offered only if the item has at least one file with
`watch: "watched"` that may be deleted right now. Otherwise the action is
absent, and the client does not have to decide for itself whether there is
anything to delete.

`set_priority` is offered only for a file of a live torrent that is not yet
fully downloaded and not excluded, and not during a data check: at that time
qBittorrent accepts a priority change and silently does not apply it.

While the item is `checking` because qBittorrent checks or moves the torrent on
its own (a recheck started from its Web UI or after a restart, moving files),
`delete`, `enable` and `delete_watched` are not offered either: the operations
would refuse such a torrent. A check that one of torrmedia's own operations
runs keeps `delete` and `enable` on offer; they queue behind it.

`enable` is offered even while other episodes of the same item are being
deleted (`delete`, `delete_watched`): "deleted the watched one, queued the next
one" is the normal flow. Such an enable queues behind the delete batch
(operation phase `waiting`) and then proceeds as usual: a check of free space,
which the batch has just freed, and starting the torrent, even if it was
stopped before the batch. Behind any other operation on the item (`start`,
`stop`, `recheck`, a priority change, removal), `enable` is not offered.

`play` is not offered for a file that a running operation is deleting (episode
deletion, the watched batch, removing the item together with its data): such a
file can be zeroed at any moment, and promising playback would be dishonest.
The item's other ready files can be played during the operation: neither
stopping, nor priority changes, nor freeing a neighbour's space, nor a recheck
changes their data. `play` is also not offered for a file outside the media
root.

An item that shares its folder or one of its files with another torrent (a
common path or a hardlink) keeps only `clear_viewed`, and a file that belongs
to more than one torrent keeps only `play`, `set_viewed` and `clear_viewed`:
there is no way to address them separately. When two torrents share the item's
folder, every file of it counts as shared.

---

## Commands

Everything that touches qBittorrent responds `202 Accepted` with the operation
as the body and a `Location: /operations/{id}` header. Viewed marks touch only
local state and respond `204 No Content`.

| Method | Path | Body | Response |
|---|---|---|---|
| `POST` | `/items` | `{"magnet": "...", "category": "Shows"}` | `202` |
| `POST` | `/items/{item}/start` | — | `202` |
| `POST` | `/items/{item}/stop` | — | `202` |
| `POST` | `/items/{item}/recheck` | — | `202` |
| `DELETE` | `/items/{item}[?delete_files=true]` | — | `202` |
| `DELETE` | `/items/{item}/files?watched=true` | — | `202` |
| `DELETE` | `/items/{item}/files/{file}` | — | `202` |
| `POST` | `/items/{item}/files/{file}/enable` | — | `202` |
| `PUT` | `/items/{item}/files/{file}/priority` | `{"priority": "high"}` | `202` |
| `PUT` | `/items/{item}/files/{file}/viewed` | `{"state": "watched"}` | `204` |
| `DELETE` | `/items/{item}/files/{file}/viewed` | — | `204` |
| `DELETE` | `/items/{item}/viewed` | — | `204` |

`POST /items` accepts only `magnet:`. http(s) links are rejected with
`403 refused`, and so is a magnet with line breaks or other control characters.

`DELETE /items/{item}` removes the torrent and keeps the files.
`?delete_files=true` deletes the data too; it is a separate explicit flag so
that a stray request cannot wipe the library. In that case the server responds
`409 streaming` if any file of the item is playing right now.

Deleting a file of a live torrent means excluding it from the torrent and
freeing its space while keeping the boundaries of neighbouring pieces. The
client does not see this mechanism: it asks to delete an episode.

`DELETE /items/{item}/files?watched=true` deletes all watched episodes of the
item at once. Episodes not watched to the end are not touched: the set
includes only `watch: "watched"`, and only files that may be deleted
individually. `watched=true` is required: without it the request would read as
"delete all files", and there is no such action. An episode that a player is
playing at that moment goes into the operation's `warnings` instead of failing
the rest.

### Operations

```json
{
  "id": "op_4f2b91c0a3d7",
  "kind": "file.delete",
  "item": "p:c2hvd3M…",
  "file": "ZTAxLm1rdg",
  "state": "running",
  "phase": "trimming",
  "started_at": 1785000000,
  "finished_at": null,
  "error": null,
  "warnings": [],
  "result": null
}
```

`state`: `queued` → `running` → `done` or `failed`.
`phase` is a human-readable stage, `null` while the operation is queued and
once it has finished. Stages that do not apply are skipped: a stopped torrent
is neither stopped nor restarted.

| `kind` | `phase` |
|---|---|
| `file.delete`, `item.delete_watched` | `waiting` (the debounce window) → `stopping` → `trimming` → `rechecking` → `restarting`; `deleting` for an item with no torrent |
| `file.enable` | `waiting` (behind a delete batch) → `enabling` → `starting` |
| `item.start` · `item.stop` · `item.recheck` | `starting` · `stopping` · `rechecking` |
| `file.priority` | `prioritizing` |
| `item.remove`, `item.remove_with_data` | `removing`; `deleting` for an item with no torrent |
| `item.add` | `adding` |

`warnings` can be non-empty even with `done`: the operation succeeded, with
caveats; for example, the data was freed but the torrent could not be
restarted. A warning about one file of a delete batch (say, its data was kept
on disk) reaches only that file's operation.

`error` and `warnings` refer to files by name or by their path inside the
library and describe states in torrmedia's own words: they carry no infohash,
no qBittorrent file id, no raw qBittorrent state and no absolute path (the
media root is cut out). The exception is a failed request to qBittorrent
itself, whose error is passed on as it is and can include that request's URL.

Updates arrive as `operation` events in the item's stream. When adding an
item, the `item` field is empty (the item does not exist yet), and such events
go to the general `/events` stream. A one-shot `GET /operations/{id}` is also
available; the server keeps the last 200 operations (running ones are never
dropped), so that a client reconnecting after a disconnect can learn the
outcome.

`result` is free-form, for diagnostics. The client does not need to and should
not rely on it: everything that matters for the interface arrives in the state.
The result of a file deletion describes its whole batch and names the files by
`name` only, with no infohash and no paths.

File deletions on one item that arrive in a row are merged into one batch
(`TORRMEDIA_DELETE_DEBOUNCE`): several selected episodes need only one stop,
one recheck and one start. Each operation still stays separate and gets its own
result.

That is why `delete` stays available while another operation is running on the
item (except while the item is being removed): otherwise there would be no way
to build up a batch. Of the other actions only `enable` (behind deletes and
other enables, see above), `play` for files that are not being deleted, and
viewed marks stay on offer during that time.

---

## Drafts

You can add a whole torrent without looking via `POST /items`. But usually you
want to see the contents and pick episodes before downloading, and a magnet
link has no contents until the metadata is downloaded. That is what a draft is
for.

A draft is a torrent already added to qBittorrent to get its file list, but not
yet confirmed. It is **not visible in the state**: it is absent from `/items`
and `/events` until the user presses "download". All it gets to take up is the
metadata and, if the torrent turned out to be fast, a few megabytes of content.

The lifecycle is described by a status, not an operation: a draft has its own
state, its own stream and its own end.

| `status` | |
|---|---|
| `resolving` | torrent added, waiting for metadata |
| `ready` | contents known, waiting for a selection |
| `confirming` | applying the selection and starting the torrent |
| `confirmed` | the item is created and visible in `/items` under `item` |
| `discarding` · `discarded` | the draft is being removed together with its data |
| `failed` | it did not work out; the reason is in `error` |

A finished draft (`confirmed`, `discarded`, `failed`) can still be read for
about 5 minutes; after that it is `404`.

### `POST /drafts`

```json
{ "magnet": "magnet:?xt=urn:btih:…", "category": "Shows" }
```

`202` and the draft body in status `resolving`, `Location: /drafts/{id}`.
`category` is optional; a category can also be assigned on confirm. The magnet
rules are those of `POST /items`.

A magnet of an existing item is rejected with `409 ambiguous`: that item has
its own files, and cancelling such a "draft" would delete them. At most 8
drafts can be open at once; one more is `409 busy`.

If the metadata does not arrive within `TORRMEDIA_DRAFT_METADATA_TIMEOUT`, the
torrent is removed together with whatever it downloaded, and the draft
`failed`.

### `GET /drafts/{draft}` · `GET /drafts/{draft}/events`

The same body, one-shot and as a stream (`event: state`, as for items). The
selection window lives on the stream: the file list shows up in it by itself.

```json
{
  "id": "dr_4f2b91c0a3d7",
  "status": "ready",
  "title": "Series.S01.2160p",
  "size": 6442450944,
  "category": null,
  "item": null,
  "error": null,
  "files": [
    { "id": "czAxZTAxLm1rdg", "name": "s01e01.mkv", "path": "s01e01.mkv",
      "size": 3221225472, "wanted": true }
  ],
  "storage": { "free": 41231234560, "total": 105551355904, "reserve": 536870912 }
}
```

`wanted` is what the server proposes to download; currently that is always the
whole file list. The client draws checkboxes from it and does not decide for
the server.

`storage` is the space on the catalog's partition. `free` and `total` are
`null` if they could not be determined. `reserve` is the margin the server keeps
free. The server does not check whether the download will fit and does not
refuse a confirm: the decision is the user's, and the interface's job is to
show the numbers before the button is pressed.

A draft file's `path` is the path inside the torrent, for displaying nesting
only; its `id` is the same one under which the file will later appear in the
item. The order of `files` is the same as the item will later return them in,
not the order inside the .torrent: some releases list episodes there out of
order.

### `POST /drafts/{draft}/confirm`

```json
{ "files": ["czAxZTAxLm1rdg", "…"], "category": "Shows" }
```

`202` and the draft in status `confirming`; `409 busy` if the draft is not
`ready`. Files not selected are excluded from the torrent, the category is
applied, the torrent is started.

`category` omitted or `null` keeps the category given on `POST /drafts`; a
string replaces it, and `""` clears it.

**An empty list means "add, but do not download yet".** The item is created
with all files set to "do not download" and stays stopped: you enable the
episodes you need later, from the item's own screen. It must not be started:
qBittorrent considers a torrent with nothing selected to be complete and seeds
nothing.

The outcome arrives in the stream: `confirmed` with `item` set to the ID of the
created item, or `failed` with a reason.

### `DELETE /drafts/{draft}`

`202` and status `discarding`: the torrent is removed **together with its
data**, that is, whatever was downloaded while the selection was being made. A
draft that is already `discarding`, `discarded` or `failed` is returned as it
is, also with `202`. A draft being confirmed is `409 busy`; a confirmed one
is `409 bad_request`: it is a regular item now, and you remove it through
`/items`.

An abandoned draft (the window was closed, the client went away, the service
was restarted) is cleaned up by the server once `TORRMEDIA_DRAFT_TTL` has passed
since it was created, if it is still `resolving` or `ready`. It goes through
`discarding` and ends `failed` with the error `draft expired`.

---

## Video

### `GET /items/{item}/files/{file}/stream/{name}`

The file body: Range/206, `ETag`, `Accept-Ranges`, correct `Content-Type`.
`HEAD` is answered too.

`{name}` is a decorative name; the server does not parse it. External players
need it: they key the playback position by the name in the URL, otherwise all
episodes merge into one. The name must be percent-encoded.

Viewing progress is counted only for a `GET` of the full file or of a single
range with an explicit start. A suffix `bytes=-N` and multi-range requests are
not counted: with the first, the player is reading trailing metadata; with the
second, the multipart framing bytes would skew the counter.

---

## Service endpoints

### `GET /health`

The only endpoint that needs no token.

```json
{ "version": "2026-11-15.2", "contract": 1,
  "checks": { "root": "ok", "viewed": "ok", "qbittorrent": "ok" } }
```

`503` if any of the checked components is not working, **but the body is still
valid**: the failing check holds its error text instead of `ok`. The client
reads `contract` from it and keeps working: qBittorrent being unavailable is
expressed by the `unknown` state and by the actions that need it dropping out
of `actions`, not by the app refusing to run. Only a `contract` mismatch is
fatal. Without `QBIT_URL` there is no `qbittorrent` check.

### `GET /categories`

```json
{ "categories": [ { "name": "Movies" }, { "name": "Shows" } ] }
```

An error comes back as a status code, not an empty list: otherwise, on an auth
failure, the client would silently send the torrent to the default category.
Without `QBIT_URL` the list is empty.

### `GET /storage`

```json
{ "free": 41231234560, "total": 105551355904, "reserve": 536870912 }
```

The same as the draft's `storage` field, for screens where there is no draft.
