# torrmedia

torrmedia is the server part of the torrmedia bundle. The bundle is this server
plus torrmedia-android, an Android TV client based on TorrServe: it streams
torrents instantly through TorrServer (a MatriX release; the Silo rewrite is not
supported) and plays the downloaded library from this server. Downloads are done
by qBittorrent.

The server is a small Go service that shows files qBittorrent has **already
downloaded** as a catalog of "item → files", serves them to a player with Range
support, and can delete a single episode from a torrent without touching the
others.

It exists because qBittorrent has no API to "delete one file of a torrent"
([#9565](https://github.com/qbittorrent/qBittorrent/issues/9565)). torrmedia
wraps the workaround: set the file to "do not download", free its data while
keeping the boundary pieces it shares with neighbouring files, and recheck the
torrent once. The client knows nothing about this hack: it just sees "delete
episode".

The service is self-contained: `curl`, any player given a direct link, or
anything else can work with it.

torrmedia is an independent project, not affiliated with or endorsed by the
authors of TorrServe, TorrServer or qBittorrent.

## How it works

```text
client ──► torrmedia ──► qBittorrent WebAPI
              │
              └──► TORRMEDIA_ROOT (reads; writes only to trim or delete)
```

The client knows one address and one API. Only torrmedia talks to qBittorrent:
it holds the credentials and it enforces the order of operations.

Three mechanisms, each doing what it is best at:

| | |
|---|---|
| **commands** | REST: resources in the path, the verb is the HTTP method, parameters in the body |
| **state** | SSE: the server pushes a snapshot on every change |
| **video** | a plain `GET` with Range, which any external player understands |

This gives three properties, which are the whole point of the design.

**The client assembles nothing itself.** The picture of the world (which items
exist, which files are downloaded, what can be done with them) is built by the
server, once, in one place. When the client used to do this by joining two
lists by file name, that is where both known data-loss scenarios came from.

**The client does not poll.** One poller queries qBittorrent for all clients,
and a snapshot is sent only when the state has changed. With polling gone,
races between parallel refreshes, stale snapshots and optimistic states go too.

**Long operations do not hang on the connection.** Everything that touches
qBittorrent responds immediately and runs in the background; progress and the
result arrive in the event stream. Client timeouts are no longer part of the
protocol.

All mutations of a torrent are strictly sequential: one per-torrent lock covers
file deletion, file enabling, priority changes, start/stop, recheck, remove,
and confirming or discarding a draft. An operation that goes through the
coordinator cannot be interrupted by another one: a `remove` cannot sneak in
between freeing a file's data and rechecking the torrent.

## Requirements

- Linux or macOS;
- a filesystem with sparse files for the library: APFS, ext4 or xfs (see
  [Filesystems](#filesystems));
- Go 1.26.8+ to build;
- qBittorrent 5.x with WebUI enabled (tested on 5.2.3 and 5.3.0, libtorrent
  2.0.13 and 2.0.15);
- on Linux: systemd, and qBittorrent running as `qbittorrent.service` with a
  `qbittorrent` group (the install script relies on both);
- on macOS: qBittorrent.app running in your user session.

Required qBittorrent settings. The service does not check them, so verify them
yourself:

| Setting | Value | Why |
|---|---|---|
| Default Save Path | inside `TORRMEDIA_ROOT` | otherwise the catalog will not see the files |
| Temp Path Enabled | `false` | with a temp path, a finished file is not where the catalog looks for it |
| Preallocation | `false` | a preallocated placeholder is indistinguishable from a finished file by its blocks |
| Default Torrent Management Mode | your choice | torrmedia does not pass `autoTMM` when adding, so this default applies; keep category save paths inside `TORRMEDIA_ROOT` too |

### Filesystems

Deleting an episode frees its space by truncating the file to zero and back to
its size, which leaves a hole on filesystems with sparse files: APFS, ext4,
xfs (on APFS a 32 MiB file went down to 0.5 MiB). On exFAT and HFS+ a delete
still works and keeps the neighbouring files' data intact, but frees no space.
The catalog also tells a finished file from a partial one by its allocated
blocks, so on such filesystems a file that is still downloading can show as
`ready`.

## Quick start

`deploy/build.sh linux|macos [amd64|arm64]` builds
`deploy/<os>/torrmedia-<version>`, where `<version>` is the `version` constant
in [main.go](main.go), and refreshes `SHA256SUMS` next to it. Linux builds
default to amd64, macOS builds to the architecture of the build machine. Each
install script installs the newest version listed in its `SHA256SUMS`, only if
the checksum matches (`TORRMEDIA_VERSION=<version>` picks another one), and
**opens nothing to the outside**: the firewall is yours.

### Linux

```bash
git clone https://github.com/yuri-nei/torrmedia.git && cd torrmedia
deploy/build.sh linux
sudo bash deploy/linux/install.sh
```

The build can run anywhere; the server only needs the `deploy/linux/`
directory. `install.sh` is idempotent and runs as root. It creates the system
user `torrmedia` and adds it to the `qbittorrent` group, writes
`/etc/torrmedia/config.env` only if it is missing, installs
`/usr/local/bin/torrmedia` with a systemd unit and a hardening drop-in, makes
the library group-writable and gives qBittorrent `UMask=0002` through a
drop-in (restarting qBittorrent the first time). The hardening drop-in allows
writes only under `/var/media` and `/var/lib/torrmedia`: if the library lives
elsewhere, edit `ReadWritePaths` in `hardening.conf`.

### macOS

```bash
deploy/build.sh macos
bash deploy/macos/install.sh    # first run: creates the config and stops
bash deploy/macos/install.sh    # after filling in the config: installs and starts
```

Run it as yourself, without `sudo`. torrmedia becomes a LaunchAgent of the
current user, in the same session and as the same user as qBittorrent.app. The binary, `config.env` and the viewed marks live in
`~/Library/Application Support/torrmedia`, the log in
`~/Library/Logs/torrmedia.log`. `bash deploy/macos/install.sh uninstall` stops
and removes the agent and leaves that folder in place.

- **Keep the library outside Desktop, Documents and Downloads** (and off
  external volumes), or grant Full Disk Access to
  `~/Library/Application Support/torrmedia/torrmedia` in System Settings >
  Privacy & Security: macOS privacy protection blocks background agents from
  those folders. The default in the generated config is `~/Movies/torrmedia`.
- **Keep the Mac awake** (System Settings > Energy) if other devices stream
  from it.
- The macOS firewall may ask whether torrmedia may accept incoming
  connections.

Check:

```bash
curl -fsS http://127.0.0.1:8091/health
```

## Configuration

`/etc/torrmedia/config.env` on Linux (read by systemd's `EnvironmentFile`),
`~/Library/Application Support/torrmedia/config.env` on macOS (passed with
`-config`). You only need to set qBittorrent access; everything else has
working defaults and does not need to appear in the config.

`-config FILE` loads `KEY=VALUE` settings in the format of systemd's
`EnvironmentFile`: one assignment per line, blank lines and `#` comments
skipped, a value may be wrapped in single or double quotes. No shell is
involved and nothing is expanded, so a password with `$` or a path with spaces
needs no escaping. A variable already set in the environment wins over the
file.

| Variable | Default | Meaning |
|---|---|---|
| `TORRMEDIA_LISTEN` | `:8091` | listen address |
| `TORRMEDIA_ROOT` | `/var/media/library` | catalog root; no operation goes outside it |
| `TORRMEDIA_TOKEN` | empty | `Authorization: Bearer <token>` on all endpoints except `/health` |
| `TORRMEDIA_HOSTS` | empty | host names clients may use besides IP addresses, `localhost`, `*.localhost` and `*.local`; comma-separated, an entry starting with `.` matches every name ending in it |
| `TORRMEDIA_VIEWED_FILE` | `/var/lib/torrmedia/viewed.json` on Linux, `torrmedia/viewed.json` in the user config directory elsewhere | viewed marks; the directory is created at startup |
| `TORRMEDIA_OPERATION_TIMEOUT` | `5m` | deadline for one operation; a recheck is awaited as long as checking in qBittorrent makes progress and abandoned only if it stalls for this long; on stop, running operations get this plus 30 s |
| `TORRMEDIA_DELETE_DEBOUNCE` | `15s` | window in which deletions are collected into one batch |
| `TORRMEDIA_STATE_POLL_INTERVAL` | `2s` | how often the server polls qBittorrent for the event stream |
| `TORRMEDIA_DRAFT_METADATA_TIMEOUT` | `3m` | how long to wait for magnet metadata in the file-selection window |
| `TORRMEDIA_DRAFT_TTL` | `15m` | how long an unconfirmed draft lives before it is cleaned up |
| `QBIT_URL` / `QBIT_USER` / `QBIT_PASS` | — | WebAPI access; without `QBIT_URL`, DELETE becomes a plain file deletion |

## Security model: read this

The service provides functionality, not a perimeter. The decisions are yours:

- **`TORRMEDIA_TOKEN` is empty by default.** Without it, anyone who can reach
  `TORRMEDIA_LISTEN` can delete files and add torrents. Either set a token or
  keep the port behind a firewall/VPN. The service logs a warning about this on
  every start.
- **A firewall or VPN does not stop requests made through your browser.** Any
  web page open on a device that can reach torrmedia can make that browser
  send requests to it. torrmedia has two defences:
  - **DNS rebinding** (a page points its own domain at torrmedia's address and
    then talks to it as if it were the same site). The server answers only to
    a `Host` that is an IP address, `localhost` or `*.localhost`, a `*.local`
    name, or a name listed in `TORRMEDIA_HOSTS`; any other name gets
    `421 misdirected`. This covers every endpoint, `/health` and streams
    included. If clients reach torrmedia by a DNS name, list it in
    `TORRMEDIA_HOSTS`.
  - **Forged requests (CSRF).** A request body must be `application/json`
    (`415` otherwise), which a page cannot send to another site without a CORS
    preflight, and this server never answers one. A state-changing request with
    `Sec-Fetch-Site: cross-site` is refused with `403`.

  What remains: a page that can guess an item's id can still send a `POST`
  without a body (start, stop, recheck, enable an episode; `DELETE` and `PUT`
  need a preflight). None of them deletes data, and a token closes this too.
- **`TORRMEDIA_LISTEN=:8091` means all interfaces.** If you only need local
  access, use `127.0.0.1:8091` with a proxy in front.
- **HTTPS is not terminated.** The token and the traffic travel in plain text;
  if you want TLS, put a reverse proxy in front.
- **The `magnet` of a private-tracker item carries your passkey.** Every item
  in the catalog includes its magnet link; the token is what keeps it from
  anyone who can reach the port.
- **`/health` needs no token.** It shows the version and, when a component is
  failing, its error text, which for qBittorrent can include its address.
- **File access.** On Linux, torrmedia is a member of the `qbittorrent` group:
  it needs this to open existing files for writing. As a side effect it can
  read any qBittorrent files that are group-readable. If you want it narrower,
  grant permissions precisely (ACLs on `TORRMEDIA_ROOT`) instead of group
  membership. On macOS it runs as your user and can read what you can.
- **Only `magnet:` links can be added.** http(s) links are rejected, and so is
  a magnet with line breaks or other control characters. A category cannot
  contain path separators.
- **Logs.** For mutating requests, only the client address, method and path
  are logged, never the request body or a magnet. On Linux the log goes to
  journald; configure its retention yourself. On macOS nothing rotates
  `~/Library/Logs/torrmedia.log`.
- **Backups.** The service does not make any. Worth backing up: `viewed.json`,
  `qBittorrent.conf` and `BT_backup`.

## API

The full reference is in [API.md](API.md). In short:

```text
GET    /events                                    state stream (SSE)
GET    /items  ·  GET /items/{item}               the same, one-shot
GET    /items/{item}/events                       one item with its files (SSE)
POST   /items                                     add a whole torrent
POST   /items/{item}/start|stop|recheck
DELETE /items/{item}[?delete_files=true]
DELETE /items/{item}/viewed                       clear the item's viewed marks
DELETE /items/{item}/files?watched=true           delete watched episodes
DELETE /items/{item}/files/{file}                 delete an episode
POST   /items/{item}/files/{file}/enable          bring an episode back
PUT    /items/{item}/files/{file}/priority        download priority
PUT    /items/{item}/files/{file}/viewed          viewed mark
DELETE /items/{item}/files/{file}/viewed          clear the viewed mark
GET    /items/{item}/files/{file}/stream/name.mkv video, Range (HEAD too)
GET    /operations/{id}                           an operation, one-shot
POST   /drafts                                    torrent contents before download
GET    /drafts/{draft}[/events]
POST   /drafts/{draft}/confirm                    download the selected files
DELETE /drafts/{draft}                            cancel, together with the data
GET    /health  ·  GET /categories  ·  GET /storage
```

The answer to "did it work" is the HTTP status code. There are no envelopes: a
success body is data, an error body is `{"error", "code"}`. Commands that touch
qBittorrent respond `202`: with an operation ID, or for drafts with the draft
itself.

IDs are opaque: the client neither parses nor constructs them. No field is an
infohash, a qBittorrent file index, a disk path or a state like `stoppedUP`.

Every item and every file has an `actions` field: the list of actions allowed
right now. The client renders what is in it and infers nothing itself. This
list and the check on incoming requests are literally one function
([permissions.go](permissions.go)), so they cannot drift apart.

## Behaviour worth knowing

- **Deletion is batched.** Several DELETEs in a row on one torrent are
  collected into one batch (`TORRMEDIA_DELETE_DEBOUNCE`) so that a single
  stop/recheck/start is enough. An invalid file in the batch gets its own
  rejection and does not cancel the others. Deleting watched episodes
  (`?watched=true`) goes through the same batch: however many episodes there
  are, the torrent is stopped and rechecked once.
- **The batch waits for the recheck to finish, and only then starts the
  torrent.** qBittorrent has two quirks. It starts a stopped torrent for the
  duration of a recheck and stops it again afterwards, and a `start` that
  arrives mid-recheck does not cancel that. Also, its API state lags by up to
  the Refresh interval (1.5 s by default), so right after a command the torrent
  is still reported as `stoppedUP`. So the server does not trust "not checking"
  until it has seen the recheck itself or 5 s have passed. It measures the wait
  from the last progress of any check in qBittorrent: a 2160p season takes
  minutes to check, and libtorrent checks one torrent at a time, so a check
  waiting for its turn sits at 0%.
- **Deleting works on a torrent qBittorrent reports as errored**, which is
  what a full disk looks like: after the stop, such a torrent counts as stopped
  once qBittorrent has had 5 s to apply it.
- **A torrent with pieces over 64 MiB cannot have episodes deleted.** The
  batch refuses it before changing anything.
- **A draft is a real torrent in qBittorrent.** To show the file list before
  downloading, the torrent is added with `stopCondition=MetadataReceived`: it
  starts, fetches the metadata and pauses itself. Until the draft is confirmed
  it carries the `torrmedia-draft` tag and is not shown in the catalog.
  Cancelling, or a draft expiring, removes the torrent **together with its
  data**, that is, whatever was downloaded while the files were being chosen.
  The tag is internal: do not put `torrmedia-draft` on torrents you want to
  keep, or the cleaner will remove them after `TORRMEDIA_DRAFT_TTL`. Data is
  deleted only after the tag is checked again, so a torrent whose tag has been
  removed is safe.
- **A file no larger than two pieces is not zeroed.** All its bytes sit in
  pieces shared with neighbouring files; there is nothing to free and something
  to break. Such a file is only excluded from the torrent, and a `warning` on
  its own operation says so.
- **A file that is being streamed right now cannot be deleted**: `409`.
- **A corrupt `viewed.json` does not block startup**: it is moved aside to
  `viewed.json.corrupt-<unixtime>` and the service starts with empty state.
- **SIGTERM is handled**: new mutations get `503`, scheduled batches are
  cancelled with a proper response to clients, and operations already started
  run to completion. torrmedia waits for them up to
  `TORRMEDIA_OPERATION_TIMEOUT` + 30 s (5.5 minutes by default), because a
  delete batch in the middle of a recheck still has to restart the torrent.
  That is why the systemd unit sets `TimeoutStopSec=6min` and the LaunchAgent
  sets `ExitTimeOut` to 360 s: the defaults (90 s and 20 s) would kill it
  halfway and leave the torrent stopped. If you raise `TORRMEDIA_OPERATION_TIMEOUT`, raise
  them too.

## Development

```bash
gofmt -l . && go vet ./... && go test -race -shuffle=on ./...
```

`regression_test.go` holds tests for scenarios that once caused data loss or
made the interface lie. The API has been rewritten since, but these tests
describe behaviour and move along with it. Do not break them without a reason.

`TestAdvertisedActionsMatchEnforcement` guards the invariant that `actions` and
the request-time check are one and the same function.
`TestEveryOfferedActionSucceeds` goes further: in every torrent state it runs
each offered action and requires it to succeed, so an action that is accepted
with `202` and then always fails inside its operation cannot slip through.

## License

GPL-3.0, see [LICENSE](LICENSE).
