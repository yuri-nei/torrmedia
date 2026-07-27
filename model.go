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
	actionRemove         = "remove"
	actionRemoveWithData = "remove_with_data"

	actionPlay        = "play"
	actionDelete      = "delete"
	actionEnable      = "enable"
	actionSetViewed   = "set_viewed"
	actionClearViewed = "clear_viewed"
)

type apiTransfer struct {
	Down  int64   `json:"down"`
	Up    int64   `json:"up"`
	ETA   *int64  `json:"eta"`
	Ratio float64 `json:"ratio"`
	Seeds int     `json:"seeds"`
	Peers int     `json:"peers"`
}

type apiItem struct {
	ID         string      `json:"id"`
	Title      string      `json:"title"`
	Status     string      `json:"status"`
	Progress   float64     `json:"progress"`
	Size       int64       `json:"size"`
	Downloaded int64       `json:"downloaded"`
	FilesTotal int         `json:"files_total"`
	FilesReady int         `json:"files_ready"`
	Category   *string     `json:"category"`
	AddedAt    int64       `json:"added_at"`
	Transfer   apiTransfer `json:"transfer"`
	Actions    []string    `json:"actions"`
}

type apiItemDetail struct {
	apiItem
	Magnet *string   `json:"magnet"`
	Files  []apiFile `json:"files"`
}

type apiFile struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Path     string   `json:"path"`
	Size     int64    `json:"size"`
	Progress float64  `json:"progress"`
	Status   string   `json:"status"`
	Watch    *string  `json:"watch"`
	Position int64    `json:"position"`
	Actions  []string `json:"actions"`
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
