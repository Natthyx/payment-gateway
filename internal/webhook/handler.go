package webhook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

	"github.com/Natthyx/payment-gateway/internal/metrics"
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
//
// Status codes are chosen for the provider's retry logic:
//
//	200 — stored (or already stored): the provider stops retrying.
//	400 — the signature does not verify: nothing is stored. Storing it would
//	      let a forged body claim a real event id, so that the genuine
//	      delivery is later dropped as a duplicate.
//	503 — we could not store it: the provider retries later.
func (h *Handler) Receive(providerName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB cap
		if err != nil {
			log.Error().Err(err).Str("provider", providerName).Msg("reading webhook body")
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		signature := webhookSignatureHeader(r, providerName)

		p, err := h.service.registry.Get(providerName)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if _, err := p.VerifyWebhook(body, signature); err != nil {
			metrics.WebhookProcessed.WithLabelValues(providerName, "rejected").Inc()
			log.Warn().Err(err).Str("provider", providerName).Msg("webhook rejected: signature verification failed")
			w.WriteHeader(http.StatusBadRequest)
			return
		}

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
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		if wh == nil {
			log.Debug().
				Str("provider", providerName).
				Str("provider_event_id", eventID).
				Msg("duplicate webhook ignored")
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
