package webhook

import (
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "io"
    "net/http"

    "github.com/go-chi/chi/v5"
    "github.com/rs/zerolog/log"

    "github.com/Natthyx/payment-gateway/internal/models"
)

type Handler struct {
    service *Service
}

func NewHandler(service *Service) *Handler {
    return &Handler{service: service}
}

func (h *Handler) Routes() func(chi.Router) {
    return func(r chi.Router) {
        r.Post("/stripe", h.Receive("stripe"))
        r.Post("/chapa", h.Receive("chapa"))
        r.Post("/telebirr", h.Receive("telebirr"))
    }
}

// Receive returns a handler for a specific provider.
// ALL webhook endpoints must return 200 immediately — even on error.
// WHY: if we return 500, the provider retries. If we return 200,
// they stop. We handle retries ourselves via the webhook worker.
func (h *Handler) Receive(providerName string) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB cap
        if err != nil {
            log.Error().Err(err).Str("provider", providerName).Msg("reading webhook body")
            w.WriteHeader(http.StatusOK)
            return
        }

        signature := webhookSignatureHeader(r, providerName)

        // Best-effort metadata extraction. The provider client knows where to
        // find event id + type in its payload shape. Used purely for dedupe
        // and observability — actual decisions still wait for VerifyWebhook
        // in the worker.
        eventID, eventType := h.extractEventMeta(providerName, body)

        // Defense in depth: if a provider does not give us a stable event id,
        // fall back to a content hash so duplicate raw bodies still dedupe.
        if eventID == "" {
            sum := sha256.Sum256(body)
            eventID = "sha256:" + hex.EncodeToString(sum[:])
        }

        wh, err := h.service.Ingest(r.Context(), ingestInput{
            Provider:        providerName,
            ProviderEventID: eventID,
            EventType:       eventType,
            Payload:         body,
            Signature:       signature,
        })
        if err != nil {
            log.Error().Err(err).Str("provider", providerName).Msg("ingesting webhook")
            w.WriteHeader(http.StatusOK)
            return
        }

        if wh == nil {
            // Duplicate event — already stored on an earlier delivery. Acknowledge.
            log.Info().
                Str("provider", providerName).
                Str("provider_event_id", eventID).
                Msg("duplicate webhook ignored")
        } else {
            log.Info().
                Int64("webhook_id", wh.ID).
                Str("provider", providerName).
                Str("event_type", eventType).
                Msg("webhook received")
        }

        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(http.StatusOK)
        _ = json.NewEncoder(w).Encode(map[string]string{"received": "ok"})
    }
}

func (h *Handler) extractEventMeta(providerName string, body []byte) (id, eventType string) {
    p, err := h.service.registry.Get(providerName)
    if err != nil {
        return "", "unknown"
    }
    meta, err := p.ParseEventMetadata(body)
    if err != nil || meta == nil {
        return "", "unknown"
    }
    if meta.EventType == "" {
        meta.EventType = "unknown"
    }
    return meta.EventID, meta.EventType
}

func webhookSignatureHeader(r *http.Request, provider string) string {
    switch provider {
    case "stripe":
        return r.Header.Get("Stripe-Signature")
    case "chapa":
        return r.Header.Get("Chapa-Signature")
    case "telebirr":
        return r.Header.Get("sign")
    default:
        return ""
    }
}

func respond(w http.ResponseWriter, status int, success bool, data interface{}, errMsg string) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    _ = json.NewEncoder(w).Encode(models.APIResponse{
        Success: success,
        Data:    data,
        Error:   errMsg,
    })
}
