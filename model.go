package main

// model.go defines what the client sees and the rules by which it is
// computed.
//
// Deliberately, not a single field here is taken from qBittorrent as is:
// states are translated into our own vocabulary, and infohashes and file
// indexes never leave the server. A client that starts parsing someone
// else's vocabulary will break on the next qBittorrent update, and it should
// not even know that qBittorrent exists.

import "strings"

// Item statuses.
const (
	statusDownloading = "downloading"
	statusSeeding     = "seeding"
	statusStopped     = "stopped"
	statusChecking    = "checking"
	statusError       = "error"
	statusShelf       = "shelf"   // no torrent, the files just sit on disk
	statusUnknown     = "unknown" // qBittorrent unreachable, state undetermined
)

// File statuses.
const (
	fileReady   = "ready"
	filePartial = "partial"
	fileSkipped = "skipped"
	fileMissing = "missing"
)

// Actions. The client renders exactly what arrives in actions and infers
// nothing on its own.
const (
	actionStart          = "start"
	actionStop           = "stop"
	actionRecheck        = "recheck"
	actionDeleteWatched  = "delete_watched"
	actionRemove         = "remove"
	actionRemoveWithData = "remove_with_data"

	actionPlay        = "play"
	actionDelete      = "delete"
	actionEnable      = "enable"
	actionSetPriority = "set_priority"
	actionSetViewed   = "set_viewed"
	actionClearViewed = "clear_viewed"
)

// File priorities are our own three-step vocabulary, not qBittorrent's
// numbers. "Do not download" is not one of them: excluding a file is done by
// delete (which frees space) and enable, which have their own mechanics, and
// a second path to the same result would diverge from them at the first
// subtlety.
const (
	priorityNormal = "normal"
	priorityHigh   = "high"
	priorityMax    = "max"
)

// priorityName translates a qBittorrent number into our vocabulary. 0 means
// the file is excluded and has no priority. qBittorrent uses the values 1, 6
// and 7, but libtorrent also knows the ones in between: anything below
// "high" is normal.
func priorityName(p int) string {
	switch {
	case p <= 0:
		return ""
	case p >= 7:
		return priorityMax
	case p == 6:
		return priorityHigh
	default:
		return priorityNormal
	}
}

// priorityValue is the reverse translation; ok=false for an unknown word.
func priorityValue(name string) (int, bool) {
	switch name {
	case priorityNormal:
		return 1, true
	case priorityHigh:
		return 6, true
	case priorityMax:
		return 7, true
	}
	return 0, false
}

type apiTransfer struct {
	Down  int64   `json:"down"`
	Up    int64   `json:"up"`
	ETA   *int64  `json:"eta"`
	Ratio float64 `json:"ratio"`
	Seeds int     `json:"seeds"`
	Peers int     `json:"peers"`
}

type apiItem struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	Status       string  `json:"status"`
	Progress     float64 `json:"progress"`
	Size         int64   `json:"size"`
	Downloaded   int64   `json:"downloaded"`
	FilesTotal   int     `json:"files_total"`
	FilesReady   int     `json:"files_ready"`
	FilesWatched int     `json:"files_watched"`
	Category     *string `json:"category"`
	// Magnet is in the list too: the id is opaque, so the client has nothing
	// to build a link from, and "share" has to work right from the list.
	Magnet   *string     `json:"magnet"`
	AddedAt  int64       `json:"added_at"`
	Transfer apiTransfer `json:"transfer"`
	Actions  []string    `json:"actions"`
}

type apiItemDetail struct {
	apiItem
	Files []apiFile `json:"files"`
}

type apiFile struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Path     string  `json:"path"`
	Size     int64   `json:"size"`
	Progress float64 `json:"progress"`
	Status   string  `json:"status"`
	// Priority is null if the file is excluded or the item has no torrent.
	Priority *string  `json:"priority"`
	Watch    *string  `json:"watch"`
	Position int64    `json:"position"`
	Actions  []string `json:"actions"`
}

// apiStorage is the space on the catalog's partition. Free and Total are
// pointers: null means "could not find out", and it is more honest for the
// client to show nothing than to show zeros. Reserve is the margin the server
// keeps free when downloading.
type apiStorage struct {
	Free    *int64 `json:"free"`
	Total   *int64 `json:"total"`
	Reserve int64  `json:"reserve"`
}

// apiDraft is a torrent the user is still only considering: its contents are
// already known, but what to download from it has not been decided. See
// drafts.go.
type apiDraft struct {
	ID       string  `json:"id"`
	Status   string  `json:"status"`
	Title    string  `json:"title"`
	Size     int64   `json:"size"`
	Category *string `json:"category"`
	// Item appears together with confirmed: the item is visible in the catalog
	// under this id.
	Item    *string        `json:"item"`
	Error   *string        `json:"error"`
	Files   []apiDraftFile `json:"files"`
	Storage apiStorage     `json:"storage"`
}

// apiDraftFile is a file of a draft. It has neither status nor progress:
// nothing is on disk yet, and the only thing to decide about it is whether
// to download it.
type apiDraftFile struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Wanted bool   `json:"wanted"`
}

type apiItemList struct {
	Items []apiItem `json:"items"`
	// Degraded explains why part of the data is unreliable. The client does
	// not have to parse it: unavailability is already reflected by empty
	// actions and the unknown status; this field is for humans.
	Degraded *string `json:"degraded"`
}

// normalizeState translates a qBittorrent state into our vocabulary.
// qBittorrent's list of values is open-ended and changes between versions,
// so an unknown value deliberately goes to error instead of failing the
// parse.
func normalizeState(state string) string {
	switch {
	case state == "":
		return statusUnknown
	case hasPrefixFold(state, "checking"), hasPrefixFold(state, "moving"):
		return statusChecking
	case hasPrefixFold(state, "error"), hasPrefixFold(state, "missingfiles"),
		hasPrefixFold(state, "unknown"):
		return statusError
	case hasPrefixFold(state, "stopped"), hasPrefixFold(state, "paused"):
		return statusStopped
	case hasPrefixFold(state, "uploading"), hasPrefixFold(state, "stalledup"),
		hasPrefixFold(state, "queuedup"), hasPrefixFold(state, "forcedup"):
		return statusSeeding
	default:
		return statusDownloading
	}
}

const (
	watchStarted = "started"
	watchWatched = "watched"
)

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}
