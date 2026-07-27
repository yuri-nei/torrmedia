package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
)

const (
	codeBadRequest          = "bad_request"
	codeUnauthorized        = "unauthorized"
	codeRefused             = "refused"
	codeNotFound            = "not_found"
	codeAmbiguous           = "ambiguous"
	codeBusy                = "busy"
	codeStreaming           = "streaming"
	codeInsufficientStorage = "insufficient_storage"
	codeQbitUnavailable     = "qbittorrent_unavailable"
	codeDeadline            = "deadline"
	codeInternal            = "internal"
	codeUnavailable         = "unavailable"
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
