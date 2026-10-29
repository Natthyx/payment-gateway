package outbound

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Natthyx/payment-gateway/internal/apierr"
	"github.com/Natthyx/payment-gateway/internal/httpx"
	"github.com/Natthyx/payment-gateway/internal/middleware"
)

type Handler struct {
	svc *Service
}

func NewHandler(s *Service) *Handler { return &Handler{svc: s} }

func (h *Handler) Routes() func(chi.Router) {
	return func(r chi.Router) {
		r.Post("/", h.Create)
		r.Get("/", h.List)
		r.Delete("/{id}", h.Delete)
	}
}

type endpointRequest struct {
	URL string `json:"url"`
}

type endpoint struct {
	ID        int64  `json:"id"`
	URL       string `json:"url"`
	IsActive  bool   `json:"is_active"`
	CreatedAt string `json:"created_at"`
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	m, ok := middleware.GetMerchant(r.Context())
	if !ok {
		httpx.Error(w, r, apierr.Unauthorized("unauthenticated"))
		return
	}
	var req endpointRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Error(w, r, apierr.InvalidRequest("invalid request body"))
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if err := validateEndpointURL(req.URL); err != nil {
		httpx.Error(w, r, err)
		return
	}
	var (
		id      int64
		active  bool
		created interface{}
	)
	err := h.svc.db.QueryRow(r.Context(), `
        INSERT INTO merchant_webhook_endpoints (merchant_id, url)
        VALUES ($1, $2)
        ON CONFLICT (merchant_id, url) DO UPDATE SET is_active = true, updated_at = NOW()
        RETURNING id, is_active, created_at
    `, m.ID, req.URL).Scan(&id, &active, &created)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, endpoint{ID: id, URL: req.URL, IsActive: active})
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	m, ok := middleware.GetMerchant(r.Context())
	if !ok {
		httpx.Error(w, r, apierr.Unauthorized("unauthenticated"))
		return
	}
	rows, err := h.svc.db.Query(r.Context(), `
        SELECT id, url, is_active, created_at::text
        FROM merchant_webhook_endpoints
        WHERE merchant_id = $1
        ORDER BY created_at DESC
    `, m.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	defer rows.Close()
	out := []endpoint{}
	for rows.Next() {
		var e endpoint
		if err := rows.Scan(&e.ID, &e.URL, &e.IsActive, &e.CreatedAt); err == nil {
			out = append(out, e)
		}
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	m, ok := middleware.GetMerchant(r.Context())
	if !ok {
		httpx.Error(w, r, apierr.Unauthorized("unauthenticated"))
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		httpx.Error(w, r, apierr.InvalidRequest("invalid id"))
		return
	}
	res, err := h.svc.db.Exec(r.Context(), `
        UPDATE merchant_webhook_endpoints SET is_active = false
        WHERE id = $1 AND merchant_id = $2
    `, id, m.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if res.RowsAffected() == 0 {
		httpx.Error(w, r, apierr.NotFound("endpoint not found"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validateEndpointURL(s string) error {
	if s == "" {
		return apierr.InvalidRequest("url is required")
	}
	u, err := url.Parse(s)
	if err != nil {
		return apierr.InvalidRequest("url is malformed")
	}
	if u.Scheme != "https" {
		return apierr.InvalidRequest("url must be https")
	}
	if u.Host == "" {
		return apierr.InvalidRequest("url must have a host")
	}
	return nil
}
