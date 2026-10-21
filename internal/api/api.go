// Package api exposes the versioned HTTP coordination API.
// It delegates every mutation and its operation ID to core.Service.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/samjingwen/tandem/core"
)

const (
	maxBodyBytes   = 1 << 20
	requestTimeout = 30 * time.Second
)

// Options configures API authentication. Bind and TLS policy belong to the server.
type Options struct {
	Token string
}

type handler struct {
	service core.Service
	token   string
}

// New returns a handler without starting a server, scheduler, or runtime.
func New(service core.Service, options Options) http.Handler {
	return &handler{service: service, token: options.Token}
}

type errorResponse struct {
	Error core.Error `json:"error"`
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		writeError(w, http.StatusNotFound, "not_found", "API route not found")
		return
	}
	if !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="tandem"`)
		writeError(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
		if !sameOrigin(r) {
			writeError(w, http.StatusForbidden, "cross_origin", "cross-origin mutations are not allowed")
			return
		}
		ids := r.Header.Values("X-Operation-ID")
		if len(ids) != 1 || !operationID.MatchString(ids[0]) {
			writeError(w, http.StatusBadRequest, "invalid_operation_id", "X-Operation-ID must contain 1-128 ASCII letters, digits, dots, underscores, colons, or hyphens, starting with a letter or digit")
			return
		}
		w.Header().Set("X-Operation-ID", ids[0])
	}
	if h.service == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "coordination service is unavailable")
		return
	}
	h.route(w, r)
}

var operationID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func (h *handler) authorized(r *http.Request) bool {
	if h.token == "" {
		return true
	}
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	got, want := sha256.Sum256([]byte(token)), sha256.Sum256([]byte(h.token))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// Forwarded headers are deliberately not trusted. A reverse proxy must preserve
// the external Host/TLS origin or enforce its own same-origin policy.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true // Non-browser clients do not need to send Origin.
	}
	if len(origins) != 1 {
		return false
	}
	origin, err := url.Parse(origins[0])
	if err != nil || origin.User != nil || origin.Host == "" || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return origin.Scheme == scheme && strings.EqualFold(origin.Host, r.Host)
}

func decode[T any](w http.ResponseWriter, r *http.Request, optional bool) (T, bool) {
	var zero T
	if r.Header.Get("Content-Type") != "" {
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
			return zero, false
		}
	} else if !optional || r.ContentLength != 0 {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return zero, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var value *T
	if err := decoder.Decode(&value); err != nil {
		if optional && errors.Is(err, io.EOF) {
			return zero, true
		}
		decodeError(w, err)
		return zero, false
	}
	if value == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "JSON body must be an object, not null")
		return zero, false
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		decodeError(w, err)
		return zero, false
	}
	return *value, true
}

func decodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "JSON body exceeds 1 MiB")
		return
	}
	// Decoder details may contain submitted private content. Keep them off the wire.
	writeError(w, http.StatusBadRequest, "invalid_request", "body must contain exactly one JSON object with only supported fields")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	// Encode before committing headers, so invalid service JSON is a typed error.
	data, err := json.Marshal(value)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "response could not be encoded")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: core.Error{Code: code, Message: message}})
}

func serviceError(w http.ResponseWriter, err error) {
	var typed *core.Error
	if errors.As(err, &typed) {
		status := http.StatusConflict
		switch typed.Code {
		case "invalid_request", "invalid_operation_id":
			status = http.StatusBadRequest
		case "not_found":
			status = http.StatusNotFound
		case "unauthorized":
			status = http.StatusUnauthorized
		case "forbidden":
			status = http.StatusForbidden
		case "unavailable", "runtime_unavailable", "incompatible":
			status = http.StatusServiceUnavailable
		case "internal":
			writeError(w, http.StatusInternalServerError, "internal", "coordination service failed")
			return
		}
		writeError(w, status, typed.Code, typed.Message)
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusGatewayTimeout, "timeout", "request timed out; mutation outcome may be unknown")
	} else if errors.Is(err, context.Canceled) {
		writeError(w, http.StatusRequestTimeout, "canceled", "request canceled; mutation outcome may be unknown")
	} else {
		writeError(w, http.StatusInternalServerError, "internal", "coordination service failed")
	}
}

func respond[T any](w http.ResponseWriter, value T, err error) {
	if err != nil {
		serviceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}
