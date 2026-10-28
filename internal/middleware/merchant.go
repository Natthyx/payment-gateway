package middleware

import (
    "context"
    "net/http"
    "strings"

    "github.com/Natthyx/payment-gateway/internal/models"
)

type merchantContextKey string

const contextKeyMerchant merchantContextKey = "merchant"

// MerchantAuthenticator is the interface the middleware needs.
// Using an interface instead of the concrete *merchant.Service
// breaks the import cycle between middleware and merchant packages.
type MerchantAuthenticator interface {
    Authenticate(ctx context.Context, apiKey string) (*models.Merchant, error)
}

// AuthenticateMerchant validates the API key in the Authorization header
// and injects the merchant into the request context.
//
// Expected header: "Authorization: Bearer gw_live_<lookup>_<secret>"
func AuthenticateMerchant(svc MerchantAuthenticator) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            authHeader := r.Header.Get("Authorization")
            if authHeader == "" {
                writeUnauthorized(w, "missing Authorization header")
                return
            }
            parts := strings.SplitN(authHeader, " ", 2)
            if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
                writeUnauthorized(w, "malformed Authorization header")
                return
            }
            merchant, err := svc.Authenticate(r.Context(), parts[1])
            if err != nil {
                writeUnauthorized(w, "invalid API key")
                return
            }
            ctx := context.WithValue(r.Context(), contextKeyMerchant, merchant)
            next.ServeHTTP(w, r.WithContext(ctx))
        })
    }
}

// GetMerchant extracts the authenticated merchant from context.
func GetMerchant(ctx context.Context) (*models.Merchant, bool) {
    m, ok := ctx.Value(contextKeyMerchant).(*models.Merchant)
    return m, ok
}