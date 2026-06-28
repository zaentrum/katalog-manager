package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// reqCtx returns the request context (handlers carry no per-request deadline of
// their own; the server's timeouts govern). Centralised so swapping in a
// per-handler timeout later is a one-line change.
func reqCtx(r *http.Request) context.Context { return r.Context() }

// writeJSON serialises v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError emits {"error": msg} with the given status (mirrors the CAP
// controllers' error bodies).
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// asString mirrors Java's String.valueOf for the lenient JSON coercions: a JSON
// string passes through; numbers/bools are stringified; null -> nil.
func asString(o any) *string {
	if o == nil {
		return nil
	}
	switch v := o.(type) {
	case string:
		return &v
	case json.Number:
		s := v.String()
		return &s
	case bool:
		s := "false"
		if v {
			s = "true"
		}
		return &s
	default:
		return nil
	}
}

// asLong coerces a JSON number or numeric string to int64 (Java asLong).
func asLong(o any) (int64, bool) {
	switch v := o.(type) {
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			// fall back via float (e.g. "12.0")
			f, ferr := v.Float64()
			if ferr != nil {
				return 0, false
			}
			return int64(f), true
		}
		return n, true
	case float64:
		return int64(v), true
	case string:
		// Long.parseLong only accepts integer strings.
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

// asDouble coerces a JSON number or numeric string to float64 (Java asDouble).
func asDouble(o any) (float64, bool) {
	switch v := o.(type) {
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case float64:
		return v, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// asInt coerces a JSON number or numeric string to int (Java asInt).
func asInt(o any) (int, bool) {
	if n, ok := asLong(o); ok {
		return int(n), true
	}
	return 0, false
}
