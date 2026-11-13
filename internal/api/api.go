// Package api exposes a vault.Vault as a REST API.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/vault"
)

// NewRouter returns an http.Handler that serves all vault operations.
// The passphrase, when needed, must be supplied via the
// X-Chunkvault-Passphrase header; a request that carries it in a query
// parameter is rejected with 400.
func NewRouter(v *vault.Vault) http.Handler {
	r := chi.NewRouter()
	r.Use(rejectPassphraseInQuery)

	r.Get("/v1/files", listHandler(v))
	r.Post("/v1/files/*", putHandler(v))
	r.Get("/v1/files/*", getHandler(v))
	r.Delete("/v1/files/*", deleteHandler(v))
	r.Get("/v1/stats", statsHandler(v))
	r.Post("/v1/gc", gcHandler(v))
	r.Post("/v1/verify", verifyHandler(v))

	return r
}

// rejectPassphraseInQuery returns 400 if the passphrase appears as a query
// parameter. The passphrase travels only in a header.
func rejectPassphraseInQuery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("passphrase") {
			http.Error(w, "passphrase must not be sent as a query parameter; use the X-Chunkvault-Passphrase header", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func putHandler(v *vault.Vault) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := fileName(r)
		if name == "" {
			http.Error(w, "file name is required", http.StatusBadRequest)
			return
		}
		stats, err := v.Put(r.Context(), name, r.Body)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, stats)
	}
}

func getHandler(v *vault.Vault) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := fileName(r)
		if name == "" {
			http.Error(w, "file name is required", http.StatusBadRequest)
			return
		}
		sw := &statusWriter{ResponseWriter: w}
		w.Header().Set("Content-Type", "application/octet-stream")
		if err := v.Get(r.Context(), name, sw); err != nil {
			if !sw.written {
				// Nothing flushed yet — we can still send an error response.
				w.Header().Del("Content-Type")
				writeError(w, err)
			}
		}
	}
}

// statusWriter wraps an http.ResponseWriter to track whether Write has been
// called, so the get handler knows if it can still send an error response.
type statusWriter struct {
	http.ResponseWriter
	written bool
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	sw.written = true
	return sw.ResponseWriter.Write(b)
}

func deleteHandler(v *vault.Vault) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := fileName(r)
		if name == "" {
			http.Error(w, "file name is required", http.StatusBadRequest)
			return
		}
		if err := v.Remove(name); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func listHandler(v *vault.Vault) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entries, err := v.List(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		if entries == nil {
			entries = []vault.Entry{}
		}
		writeJSON(w, http.StatusOK, entries)
	}
}

func statsHandler(v *vault.Vault) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, err := v.Stats(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s)
	}
}

func gcHandler(v *vault.Vault) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dryRun, _ := strconv.ParseBool(r.URL.Query().Get("dry_run"))
		st, err := v.GC(r.Context(), dryRun)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

func verifyHandler(v *vault.Vault) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		full, _ := strconv.ParseBool(r.URL.Query().Get("full"))
		rep, err := v.Verify(r.Context(), full)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rep)
	}
}

// fileName extracts the URL path parameter "name", supporting slashes.
func fileName(r *http.Request) string {
	// chi encodes the wildcard; chi.URLParam gives a single segment.
	// We take everything after /v1/files/ to support slashed names.
	const prefix = "/v1/files/"
	if strings.HasPrefix(r.URL.Path, prefix) {
		return strings.TrimPrefix(r.URL.Path, prefix)
	}
	return chi.URLParam(r, "name")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	msg := err.Error()
	code := http.StatusInternalServerError
	if strings.Contains(msg, "not found") {
		code = http.StatusNotFound
	} else if strings.Contains(msg, "not initialized") {
		code = http.StatusServiceUnavailable
	}
	http.Error(w, fmt.Sprintf(`{"error":%q}`, msg), code)
}

