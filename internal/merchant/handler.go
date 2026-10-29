package merchant

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Natthyx/payment-gateway/internal/apierr"
	"github.com/Natthyx/payment-gateway/internal/audit"
	"github.com/Natthyx/payment-gateway/internal/httpx"
	"github.com/Natthyx/payment-gateway/internal/middleware"
	"github.com/Natthyx/payment-gateway/internal/models"
)

type Handler struct {
	service *Service
	audit   *audit.Service
}

func NewHandler(service *Service, auditSvc *audit.Service) *Handler {
	return &Handler{service: service, audit: auditSvc}
}

func (h *Handler) Routes() func(chi.Router) {
	return func(r chi.Router) {
		r.Post("/", h.Create)
		r.Group(func(r chi.Router) {
			r.Use(middleware.AuthenticateMerchant(h.service))
			r.Get("/me", h.Me)
			r.Post("/me/regenerate-key", h.RegenerateKey)
		})
	}
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req models.CreateMerchantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Error(w, r, apierr.InvalidRequest("invalid request body"))
		return
	}
	resp, err := h.service.Create(r.Context(), req)
	if err != nil {
		httpx.Error(w, r, mapErr(err))
		return
	}
	h.audit.Record(r.Context(), audit.Entry{
		MerchantID: &resp.Merchant.ID,
		Actor:      audit.MerchantActor(resp.Merchant.ID),
		Action:     "merchant.create",
		Target:     audit.MerchantActor(resp.Merchant.ID),
		IP:         r.RemoteAddr,
		UserAgent:  r.UserAgent(),
		RequestID:  r.Header.Get("X-Request-ID"),
		Details:    map[string]interface{}{"email": resp.Merchant.Email},
	})
	httpx.JSON(w, http.StatusCreated, resp)
}

func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	m, ok := middleware.GetMerchant(r.Context())
	if !ok {
		httpx.Error(w, r, apierr.Unauthorized("unauthenticated"))
		return
	}
	httpx.JSON(w, http.StatusOK, m)
}

func (h *Handler) RegenerateKey(w http.ResponseWriter, r *http.Request) {
	m, ok := middleware.GetMerchant(r.Context())
	if !ok {
		httpx.Error(w, r, apierr.Unauthorized("unauthenticated"))
		return
	}
	key, err := h.service.RegenerateKey(r.Context(), m.ID)
	if err != nil {
		httpx.Error(w, r, mapErr(err))
		return
	}
	h.audit.Record(r.Context(), audit.Entry{
		MerchantID: &m.ID,
		Actor:      audit.MerchantActor(m.ID),
		Action:     "api_key.rotate",
		Target:     audit.MerchantActor(m.ID),
		IP:         r.RemoteAddr,
		UserAgent:  r.UserAgent(),
		RequestID:  r.Header.Get("X-Request-ID"),
	})
	httpx.JSON(w, http.StatusOK, map[string]string{
		"api_key": key,
		"message": "store this key securely — it will not be shown again",
	})
}

// mapErr translates merchant service sentinel errors into structured API errors.
func mapErr(err error) error {
	switch {
	case errors.Is(err, ErrMissingRequired):
		return apierr.InvalidRequest(err.Error())
	case errors.Is(err, ErrEmailAlreadyTaken):
		return apierr.Conflict(err.Error())
	case errors.Is(err, ErrMerchantNotFound):
		return apierr.NotFound(err.Error())
	case errors.Is(err, ErrMerchantInactive), errors.Is(err, ErrInvalidAPIKey):
		return apierr.Unauthorized(err.Error())
	default:
		return err
	}
}
