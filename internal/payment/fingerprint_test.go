package payment

import (
    "encoding/json"
    "testing"

    "github.com/Natthyx/payment-gateway/internal/models"
)

func TestRequestFingerprint(t *testing.T) {
    base := models.CreatePaymentIntentRequest{Amount: 1050, Currency: "USD", Provider: "stripe",
        Metadata: json.RawMessage(`{"order": 1}`)}
    same := base
    same.Metadata = json.RawMessage(`{"order":1}`) // whitespace only
    if requestFingerprint(base) != requestFingerprint(same) {
        t.Fatal("whitespace in metadata changed the fingerprint")
    }
    other := base
    other.Amount = 1051
    if requestFingerprint(base) == requestFingerprint(other) {
        t.Fatal("different amount, same fingerprint")
    }
    other = base
    other.Provider = "chapa"
    if requestFingerprint(base) == requestFingerprint(other) {
        t.Fatal("different provider, same fingerprint")
    }
}
