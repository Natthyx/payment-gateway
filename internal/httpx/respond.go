// Package httpx is the single source of truth for response shape.
package httpx

import (
    "encoding/json"
    "errors"
    "net/http"

    "github.com/rs/zerolog/log"

    "github.com/Natthyx/payment-gateway/internal/apierr"
    "github.com/Natthyx/payment-gateway/internal/models"
)

// JSON writes a successful envelope with HTTP `status`.
func JSON(w http.ResponseWriter, status int, data interface{}) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    _ = json.NewEncoder(w).Encode(models.APIResponse{Success: true, Data: data})
}

// JSONWithMeta writes a paginated envelope.
func JSONWithMeta(w http.ResponseWriter, status int, data interface{}, meta *models.Meta) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    _ = json.NewEncoder(w).Encode(models.APIResponse{Success: true, Data: data, Meta: meta})
}

// Error converts any error into a structured response. apierr.Error is rendered
// as-is; anything else becomes an internal error and is logged with full detail.
func Error(w http.ResponseWriter, r *http.Request, err error) {
    var apiErr *apierr.Error
    if errors.As(err, &apiErr) {
        write(w, apiErr)
        return
    }
    log.Error().Err(err).Str("path", r.URL.Path).Msg("internal error")
    write(w, apierr.Internal("internal error"))
}

func write(w http.ResponseWriter, e *apierr.Error) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(e.Status)
    _ = json.NewEncoder(w).Encode(models.APIResponse{Success: false, ErrorObj: e})
}
