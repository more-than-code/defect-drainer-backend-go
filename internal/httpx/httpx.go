// Package httpx is thin JSON helpers over net/http (no extra router).
package httpx

import (
	"encoding/json"
	"net/http"
)

// WriteJSON writes v as application/json.
func WriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes {"error": msg}.
func WriteError(w http.ResponseWriter, code int, msg string) {
	WriteJSON(w, code, map[string]string{"error": msg})
}

// DecodeJSON reads a JSON body capped at maxBytes.
func DecodeJSON(r *http.Request, maxBytes int64, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	return dec.Decode(dst)
}
