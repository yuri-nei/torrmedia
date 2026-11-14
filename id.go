package main

// id.go holds the opaque identifiers of items and files.
//
// The client never parses them or derives anything from them. That is the
// price of freedom: the server can change its internal addressing (infohash,
// qBittorrent file index, on-disk path) without touching the contract.
//
// Inside is a path relative to the catalog root rather than an infohash,
// because the path survives both adding the torrent and removing it: the file
// on disk stays the same file. The infohash is used only for items that live
// outside the root, since they have no catalog path at all.
//
// Unpadded base64url was chosen so that an id contains neither slashes nor
// characters that need escaping: the id lives in a URL path segment, and any
// %2F normalization by intermediaries would break addressing.

import (
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
)

type idKind byte

const (
	idPath idKind = 'p' // value: a path relative to the catalog root
	idHash idKind = 't' // value: infohash of a torrent outside the catalog root
)

var errBadID = errors.New("malformed id")

type objectID struct {
	kind  idKind
	value string
}

func (o objectID) String() string {
	return string(o.kind) + ":" + base64.RawURLEncoding.EncodeToString([]byte(o.value))
}

func pathID(rel string) string { return objectID{kind: idPath, value: filepath.ToSlash(rel)}.String() }

func hashID(hash string) string { return objectID{kind: idHash, value: hash}.String() }

func parseID(raw string) (objectID, error) {
	kind, encoded, found := strings.Cut(raw, ":")
	if !found || len(kind) != 1 {
		return objectID{}, errBadID
	}
	switch idKind(kind[0]) {
	case idPath, idHash:
	default:
		return objectID{}, errBadID
	}
	value, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(value) == 0 || strings.ContainsRune(string(value), 0) {
		return objectID{}, errBadID
	}
	return objectID{kind: idKind(kind[0]), value: string(value)}, nil
}

// fileID addresses a file within an item, also by path, relative to the item
// itself. The qBittorrent file index won't do: it exists only as long as the
// torrent does, while the file on disk outlives the torrent's removal.
//
// There is deliberately no inverse function: an id that came from the client
// is never turned into a path, only compared against catalog values.
func fileID(relWithinItem string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(filepath.ToSlash(relWithinItem)))
}
