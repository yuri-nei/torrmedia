package main

import "strings"

const (
	statusDownloading = "downloading"
	statusSeeding     = "seeding"
	statusStopped     = "stopped"
	statusChecking    = "checking"
	statusError       = "error"
	statusShelf       = "shelf"
	statusUnknown     = "unknown"
)

const (
	fileReady   = "ready"
	filePartial = "partial"
	fileSkipped = "skipped"
	fileMissing = "missing"
)

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

const (
	priorityNormal = "normal"
	priorityHigh   = "high"
	priorityMax    = "max"
)

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
	ID           string      `json:"id"`
	Title        string      `json:"title"`
	Status       string      `json:"status"`
	Progress     float64     `json:"progress"`
	Size         int64       `json:"size"`
	Downloaded   int64       `json:"downloaded"`
	FilesTotal   int         `json:"files_total"`
	FilesReady   int         `json:"files_ready"`
	FilesWatched int         `json:"files_watched"`
	Category     *string     `json:"category"`
	Magnet       *string     `json:"magnet"`
	AddedAt      int64       `json:"added_at"`
	Transfer     apiTransfer `json:"transfer"`
	Actions      []string    `json:"actions"`
}

type apiItemDetail struct {
	apiItem
	Files []apiFile `json:"files"`
}

type apiFile struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Path     string   `json:"path"`
	Size     int64    `json:"size"`
	Progress float64  `json:"progress"`
	Status   string   `json:"status"`
	Priority *string  `json:"priority"`
	Watch    *string  `json:"watch"`
	Position int64    `json:"position"`
	Actions  []string `json:"actions"`
}

type apiStorage struct {
	Free    *int64 `json:"free"`
	Total   *int64 `json:"total"`
	Reserve int64  `json:"reserve"`
}

type apiDraft struct {
	ID       string         `json:"id"`
	Status   string         `json:"status"`
	Title    string         `json:"title"`
	Size     int64          `json:"size"`
	Category *string        `json:"category"`
	Item     *string        `json:"item"`
	Error    *string        `json:"error"`
	Files    []apiDraftFile `json:"files"`
	Storage  apiStorage     `json:"storage"`
}

type apiDraftFile struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Wanted bool   `json:"wanted"`
}

type apiItemList struct {
	Items    []apiItem `json:"items"`
	Degraded *string   `json:"degraded"`
}

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
