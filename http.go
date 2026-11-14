package main

// http.go holds the shared response plumbing.
//
// There are no {"ok": …} envelopes here: the answer to "did it work" is the
// HTTP status code. A success body is the data, an error body is
// {error, code}. One rule, with nothing for the client to forget to check.

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
)

// Machine-readable error codes. The text is written for humans and may
// change; the code is part of the contract.
const (
	codeBadRequest      = "bad_request"
	codeUnauthorized    = "unauthorized"
	codeRefused         = "refused"
	codeNotFound        = "not_found"
	codeAmbiguous       = "ambiguous"
	codeBusy            = "busy"
	codeStreaming       = "streaming"
	codeQbitUnavailable = "qbittorrent_unavailable"
	codeInternal        = "internal"
	codeUnavailable     = "unavailable"
)

type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeErr(w http.ResponseWriter, status int, code, format string, a ...any) {
	writeJSON(w, status, apiError{Error: fmt.Sprintf(format, a...), Code: code})
}

func writeNoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// decodeBody reads the JSON request body. An empty body is an empty object:
// commands without parameters have no body at all.
func decodeBody(r *http.Request, v any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return fmt.Errorf("body: %w", err)
	}
	return nil
}

func (s *server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" && r.URL.Path != "/health" {
			// Constant-time comparison: a plain != returns at the first
			// mismatching byte and makes the token guessable by timing.
			expected := "Bearer " + s.token
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(expected)) != 1 {
				writeErr(w, http.StatusUnauthorized, codeUnauthorized, "missing or invalid token")
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if s.draining.Load() {
				writeErr(w, http.StatusServiceUnavailable, codeUnavailable,
					"torrmedia is shutting down")
				return
			}
			log.Printf("%s %s %s", r.RemoteAddr, r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}
