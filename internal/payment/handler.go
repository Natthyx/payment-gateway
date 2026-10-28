package payment

import (
    "encoding/json"
    "net/http"
    "strconv"

    "github.com/go-chi/chi/v5"

    "github.com/Natthyx/payment-gateway/internal/apierr"
    "github.com/Natthyx/payment-gateway/internal/httpx"
    "github.com/Natthyx/payment-gateway/internal/ledger"
    "github.com/Natthyx/payment-gateway/internal/middleware"
    "github.com/Natthyx/payment-gateway/internal/models"
)

type Handler struct {
    service *Service
    refunds *RefundService
    ledger  *ledger.Service
}

func NewHandler(service *Service, refunds *RefundService, ledgerSvc *ledger.Service) *Handler {
    return &Handler{service: service, refunds: refunds, ledger: ledgerSvc}
}

func (h *Handler) Routes() func(chi.Router) {
    return func(r chi.Router) {
        r.Post("/", h.Create)
        r.Get("/", h.List)
        r.Get("/{id}", h.Get)
        r.Get("/{id}/ledger", h.GetLedger)
        r.Post("/{id}/refunds", h.CreateRefund)
    }
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
    m, ok := middleware.GetMerchant(r.Context())
    if !ok {
        httpx.Error(w, r, apierr.Unauthorized("unauthenticated"))
        return
    }
    var req models.CreatePaymentIntentRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        httpx.Error(w, r, apierr.InvalidRequest("invalid request body"))
        return
    }
    if k := r.Header.Get("Idempotency-Key"); k != "" {
        req.IdempotencyKey = k
    }
    if err := ValidateCreatePaymentIntent(&req); err != nil {
        httpx.Error(w, r, err)
        return
    }
    resp, err := h.service.CreateIntent(r.Context(), m.ID, req)
    if err != nil {
        httpx.Error(w, r, err)
        return
    }
    httpx.JSON(w, http.StatusCreated, resp)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
    m, ok := middleware.GetMerchant(r.Context())
    if !ok {
        httpx.Error(w, r, apierr.Unauthorized("unauthenticated"))
        return
    }
    id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
    if err != nil {
        httpx.Error(w, r, apierr.InvalidRequest("invalid payment id"))
        return
    }
    pi, err := h.service.GetIntent(r.Context(), m.ID, id)
    if err != nil {
        httpx.Error(w, r, err)
        return
    }
    httpx.JSON(w, http.StatusOK, pi)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
    m, ok := middleware.GetMerchant(r.Context())
    if !ok {
        httpx.Error(w, r, apierr.Unauthorized("unauthenticated"))
        return
    }
    page, _ := strconv.Atoi(r.URL.Query().Get("page"))
    limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
    intents, total, err := h.service.ListIntents(r.Context(), m.ID, page, limit)
    if err != nil {
        httpx.Error(w, r, err)
        return
    }
    httpx.JSONWithMeta(w, http.StatusOK, intents, &models.Meta{Total: total, Page: page, Limit: limit})
}

func (h *Handler) GetLedger(w http.ResponseWriter, r *http.Request) {
    m, ok := middleware.GetMerchant(r.Context())
    if !ok {
        httpx.Error(w, r, apierr.Unauthorized("unauthenticated"))
        return
    }
    id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
    if err != nil {
        httpx.Error(w, r, apierr.InvalidRequest("invalid payment id"))
        return
    }
    if _, err := h.service.GetIntent(r.Context(), m.ID, id); err != nil {
        httpx.Error(w, r, err)
        return
    }
    entries, err := h.ledger.GetEntriesForPayment(r.Context(), id)
    if err != nil {
        httpx.Error(w, r, err)
        return
    }
    httpx.JSON(w, http.StatusOK, entries)
}

func (h *Handler) CreateRefund(w http.ResponseWriter, r *http.Request) {
    m, ok := middleware.GetMerchant(r.Context())
    if !ok {
        httpx.Error(w, r, apierr.Unauthorized("unauthenticated"))
        return
    }
    id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
    if err != nil {
        httpx.Error(w, r, apierr.InvalidRequest("invalid payment id"))
        return
    }
    var req models.CreateRefundRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        httpx.Error(w, r, apierr.InvalidRequest("invalid request body"))
        return
    }
    if k := r.Header.Get("Idempotency-Key"); k != "" {
        req.IdempotencyKey = k
    }
    rf, err := h.refunds.Create(r.Context(), m.ID, id, req)
    if err != nil {
        httpx.Error(w, r, err)
        return
    }
    httpx.JSON(w, http.StatusCreated, rf)
}
