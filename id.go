package main

import (
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
)

type idKind byte

const (
	idPath idKind = 'p'
	idHash idKind = 't'
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

func fileID(relWithinItem string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(filepath.ToSlash(relWithinItem)))
}

func parseFileID(raw string) (string, error) {
	value, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(value) == 0 || strings.ContainsRune(string(value), 0) {
		return "", errBadID
	}
	rel := filepath.ToSlash(filepath.Clean(string(value)))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
		return "", errBadID
	}
	return rel, nil
}
