package main

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
		actionStart, actionStop, actionRecheck, actionRemove, actionRemoveWithData,
		actionClearViewed,
	}
	fileActionOrder = []string{
		actionPlay, actionDelete, actionEnable, actionSetViewed, actionClearViewed,
	}
)

func (s *server) permitItem(cat *catalog, item *catalogItem, action string) *denial {
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
	needsTorrent := action != actionRemoveWithData
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
	case actionRemove, actionRemoveWithData:
	default:
		return deny(http.StatusBadRequest, codeBadRequest, "unknown action %q")
	}
	return nil
}

func (s *server) permitFile(cat *catalog, item *catalogItem, file *catalogFile, action string) *denial {
	switch action {
	case actionPlay:
		if file.status != fileReady {
			return deny(http.StatusConflict, codeNotFound, "file is not fully downloaded")
		}
		if s.operations.busy(item.id) {
			return deny(http.StatusConflict, codeBusy, "an operation is running for this item")
		}
		return nil
	case actionSetViewed:
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
	if s.operations.busy(item.id) {
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
	default:
		return deny(http.StatusBadRequest, codeBadRequest, "unknown action")
	}
	return nil
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
