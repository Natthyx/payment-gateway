package outbound

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestSend_SignsWithSecret(t *testing.T) {
	secret := "super_secret"
	body := []byte(`{"event_type":"payment.succeeded"}`)

	var captured string
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("X-Gateway-Signature")
		capturedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewService(nil, secret)
	status, err := s.send(t.Context(), srv.URL, body, 7)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	// Verify signature
	parts := strings.Split(captured, ",")
	if len(parts) != 2 {
		t.Fatalf("bad sig header: %q", captured)
	}
	ts := strings.TrimPrefix(parts[0], "t=")
	sig := strings.TrimPrefix(parts[1], "v1=")
	if _, err := strconv.ParseInt(ts, 10, 64); err != nil {
		t.Fatalf("bad ts: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(capturedBody)))
	if hex.EncodeToString(mac.Sum(nil)) != sig {
		t.Fatalf("signature mismatch")
	}
}

func TestNextBackoff_Caps(t *testing.T) {
	if nextBackoff(20) > maxBackoff {
		t.Fatal("backoff exceeded cap")
	}
	if nextBackoff(1) != baseBackoff {
		t.Fatal("first attempt should equal base")
	}
}
