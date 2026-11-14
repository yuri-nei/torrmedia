package main

// permissions.go decides what can be done with an item and a file.
//
// The key point: the `actions` list sent to the client and the check at the
// entry of a request are ONE function. Not "kept consistent" but literally
// one. They have no way to diverge: the client cannot show a button that the
// server will refuse to execute, and vice versa.
//
// Only stable availability goes here. Momentary races, such as a file
// starting to play between rendering and the click, are not reflected here;
// otherwise the button would blink in time with the player's buffering. The
// handler itself catches such cases by answering 409.

import "net/http"

type denial struct {
	status int
	code   string
	reason string
}

func deny(status int, code, reason string) *denial {
	return &denial{status: status, code: code, reason: reason}
}

var (
	itemActionOrder = []string{
		actionStart, actionStop, actionRecheck, actionDeleteWatched,
		actionRemove, actionRemoveWithData, actionClearViewed,
	}
	fileActionOrder = []string{
		actionPlay, actionDelete, actionEnable, actionSetPriority, actionSetViewed, actionClearViewed,
	}
)

// permitItem returns nil if the action is allowed, otherwise a ready denial.
func (s *server) permitItem(cat *catalog, item *catalogItem, action string) *denial {
	// Viewed marks are our local state: they depend neither on qBittorrent,
	// nor on ambiguity, nor on a running operation.
	if action == actionClearViewed {
		if item.shelfRel == "" {
			return deny(http.StatusConflict, codeNotFound, "item is not on the shelf")
		}
		for i := range item.files {
			if item.files[i].watch != "" {
				return nil
			}
		}
		return deny(http.StatusConflict, codeBadRequest, "item has no watch marks")
	}
	// While qBittorrent is silent, we do not know whether a torrent stands
	// behind the item: it looks like plain files on disk. They must not be
	// deleted, since a torrent we cannot see would download them again at once.
	if !cat.qbitOK {
		return deny(http.StatusBadGateway, codeQbitUnavailable, "qbittorrent is unavailable")
	}
	if item.ambiguous {
		return deny(http.StatusConflict, codeAmbiguous,
			"this item shares files with another torrent; refusing to act on it")
	}
	if s.operations.busy(item.id) {
		return deny(http.StatusConflict, codeBusy, "an operation is already running for this item")
	}
	// Watched episodes can be deleted even from an item with no torrent: they
	// are the same files on disk as everywhere else.
	needsTorrent := action != actionRemoveWithData && action != actionDeleteWatched
	if needsTorrent && !item.torrentBacked() {
		return deny(http.StatusConflict, codeNotFound, "this item has no torrent behind it")
	}
	switch action {
	case actionStart:
		if item.status != statusStopped {
			return deny(http.StatusConflict, codeBusy, "item is not stopped")
		}
	case actionStop:
		if item.status != statusDownloading && item.status != statusSeeding {
			return deny(http.StatusConflict, codeBusy, "item is not running")
		}
	case actionRecheck:
		if item.status == statusChecking {
			return deny(http.StatusConflict, codeBusy, "item is already being checked")
		}
	case actionDeleteWatched:
		if len(s.watchedToDelete(cat, item)) == 0 {
			return deny(http.StatusConflict, codeNotFound, "item has no watched files to delete")
		}
	case actionRemove, actionRemoveWithData:
	default:
		return deny(http.StatusBadRequest, codeBadRequest, "unknown action %q")
	}
	return nil
}

// permitFile does the same for a file.
// enableQueuesBehind lists the item operations behind which enabling an
// episode queues up instead of being refused. "Delete the watched episode,
// queue the next one for download, go watch" is the usual sequence, but a
// delete occupies the item for the debounce window
// (TORRMEDIA_DELETE_DEBOUNCE) plus the stop and recheck, i.e. for minutes,
// and enable simply was not offered during that time: tapping the episode
// did nothing, and it never got downloaded. runFileEnable waits for the
// batch and only then touches the priority and starts the torrent.
var enableQueuesBehind = []string{opFileDelete, opItemDeleteWatched, opFileEnable}

func (s *server) permitFile(cat *catalog, item *catalogItem, file *catalogFile, action string) *denial {
	switch action {
	case actionPlay:
		if file.status != fileReady {
			return deny(http.StatusConflict, codeNotFound, "file is not fully downloaded")
		}
		// A file that a running operation is deleting may be zeroed at any
		// moment: offering it would promise something that breaks off midway.
		// The operation does not touch the item's other files (not the stop,
		// the priorities, a neighbour's trim, or the recheck, which only
		// reads), so they can be watched without waiting for the delete to
		// finish. The hard guarantee that "the player never gets zeros" comes
		// not from this rule but from the inode claims in streamRegistry; here
		// it is only about keeping the actions list honest.
		if s.operations.dooms(item.id, file.id) {
			return deny(http.StatusConflict, codeBusy, "file is being deleted")
		}
		return nil
	case actionSetViewed:
		// A mark can be set in any state, including downgrading "watched"
		// to "started". The only condition is that the file is ours.
		if file.shelfRel == "" {
			return deny(http.StatusConflict, codeNotFound, "file is not on the shelf")
		}
		return nil
	case actionClearViewed:
		if file.shelfRel == "" {
			return deny(http.StatusConflict, codeNotFound, "file is not on the shelf")
		}
		if file.watch == "" {
			return deny(http.StatusConflict, codeBadRequest, "file has no watch mark")
		}
		return nil
	}

	if !cat.qbitOK {
		return deny(http.StatusBadGateway, codeQbitUnavailable, "qbittorrent is unavailable")
	}
	// Delete deliberately does NOT look at busyness: several episodes of one
	// item accumulate in one batch, which is what the batch exists for. The
	// per-torrent coordinator ensures ordering, and a duplicate inside the batch
	// is cut off by file id. Checking busyness here would make batching
	// unreachable.
	busy := s.operations.busy(item.id)
	if action == actionEnable {
		busy = s.operations.busyBeyond(item.id, enableQueuesBehind...)
	}
	if action != actionDelete && busy {
		return deny(http.StatusConflict, codeBusy, "an operation is already running for this item")
	}
	if file.ambiguous {
		return deny(http.StatusConflict, codeAmbiguous,
			"file belongs to more than one torrent; refusing to act on it")
	}
	switch action {
	case actionDelete:
		if file.status == fileSkipped {
			return deny(http.StatusConflict, codeNotFound, "file is already excluded from the torrent")
		}
		if file.shelfRel == "" {
			return deny(http.StatusForbidden, codeRefused, "file lies outside the media root")
		}
	case actionEnable:
		if !item.torrentBacked() {
			return deny(http.StatusConflict, codeNotFound, "this item has no torrent behind it")
		}
		if file.status != fileSkipped {
			return deny(http.StatusConflict, codeBadRequest, "file is not excluded")
		}
	case actionSetPriority:
		if !item.torrentBacked() || file.qbitIndex < 0 {
			return deny(http.StatusConflict, codeNotFound, "this file has no torrent behind it")
		}
		// An excluded file needs enable first: setting a priority must not silently
		// put it back into downloading, bypassing the space check.
		if file.status == fileSkipped || file.priority == "" {
			return deny(http.StatusConflict, codeBadRequest, "file is excluded; enable it first")
		}
		// A fully downloaded file has no use for ordering anymore.
		if file.status == fileReady {
			return deny(http.StatusConflict, codeBadRequest, "file is already downloaded")
		}
		// During a check qBittorrent answers filePrio with 200 and silently
		// changes nothing.
		if item.status == statusChecking {
			return deny(http.StatusConflict, codeBusy, "item is being checked")
		}
	default:
		return deny(http.StatusBadRequest, codeBadRequest, "unknown action")
	}
	return nil
}

// watchedToDelete returns the item's watched files that may be deleted right
// now. The same list answers both "should the action be offered" and "what
// exactly to delete", so the two cannot diverge. Partly watched files never
// get in here.
func (s *server) watchedToDelete(cat *catalog, item *catalogItem) []*catalogFile {
	var out []*catalogFile
	for i := range item.files {
		file := &item.files[i]
		if file.watch != watchWatched {
			continue
		}
		if s.permitFile(cat, item, file, actionDelete) == nil {
			out = append(out, file)
		}
	}
	return out
}

func (s *server) itemActionList(cat *catalog, item *catalogItem) []string {
	actions := make([]string, 0, len(itemActionOrder))
	for _, action := range itemActionOrder {
		if s.permitItem(cat, item, action) == nil {
			actions = append(actions, action)
		}
	}
	return actions
}

func (s *server) fileActionList(cat *catalog, item *catalogItem, file *catalogFile) []string {
	actions := make([]string, 0, len(fileActionOrder))
	for _, action := range fileActionOrder {
		if s.permitFile(cat, item, file, action) == nil {
			actions = append(actions, action)
		}
	}
	return actions
}
