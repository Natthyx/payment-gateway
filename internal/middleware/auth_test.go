package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Natthyx/payment-gateway/internal/models"
)

type stubAuth struct {
	merchant *models.Merchant
	err      error
}

func (s *stubAuth) Authenticate(_ context.Context, _ string) (*models.Merchant, error) {
	return s.merchant, s.err
}

func TestAuthenticateMerchant_RejectsMissing(t *testing.T) {
	handler := AuthenticateMerchant(&stubAuth{err: errors.New("never called")})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("should not be called") }),
	)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAuthenticateMerchant_RejectsMalformed(t *testing.T) {
	handler := AuthenticateMerchant(&stubAuth{err: errors.New("never")})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("should not be called") }),
	)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Basic abc")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAuthenticateMerchant_InjectsMerchant(t *testing.T) {
	m := &models.Merchant{ID: 42, Name: "Acme"}
	auth := &stubAuth{merchant: m}
	var got *models.Merchant
	handler := AuthenticateMerchant(auth)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mm, ok := GetMerchant(r.Context())
			if !ok {
				t.Fatal("merchant not in ctx")
			}
			got = mm
			w.WriteHeader(http.StatusOK)
		}),
	)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer gw_live_xxx_yyy")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got == nil || got.ID != 42 {
		t.Fatalf("merchant wrong: %+v", got)
	}
}
