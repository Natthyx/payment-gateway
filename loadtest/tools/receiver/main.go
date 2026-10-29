// Fake merchant webhook receiver for the G6 outbound-webhook scenario.
//
//	POST /hook/flaky  fail with probability FAIL_RATE (deterministic per key+attempt)
//	POST /hook/ok     always 200
//	POST /hook/dead   always 500
//	GET  /stats       JSON summary (?full=1 adds every attempt)
//	POST /reset       clear state
//
// Every request's X-Gateway-Signature is checked against SIGNING_SECRET using
// the gateway's documented scheme: t=<unix>,v1=hex(HMAC-SHA256(secret, t+"."+body)).
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type attempt struct {
	At         time.Time `json:"at"`
	Mode       string    `json:"mode"`
	Status     int       `json:"status"`
	SigValid   bool      `json:"sig_valid"`
	SigSkewSec int64     `json:"sig_skew_s"`
	BodySHA256 string    `json:"body_sha256"`
	EventType  string    `json:"event_type"`
	IntentID   int64     `json:"payment_intent_id"`
}

type server struct {
	secret   []byte
	failRate float64
	seed     string

	mu     sync.Mutex
	byKey  map[string][]attempt
	noKey  int
	badReq int
}

func main() {
	rate, _ := strconv.ParseFloat(getenv("FAIL_RATE", "0.30"), 64)
	s := &server{
		secret:   []byte(os.Getenv("SIGNING_SECRET")),
		failRate: rate,
		seed:     getenv("FAIL_SEED", "1"),
		byKey:    map[string][]attempt{},
	}
	if len(s.secret) == 0 {
		log.Fatal("SIGNING_SECRET is required")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hook/{mode}", s.hook)
	mux.HandleFunc("GET /stats", s.stats)
	mux.HandleFunc("POST /reset", s.reset)
	log.Printf("receiver: fail_rate=%.2f seed=%s", s.failRate, s.seed)
	log.Fatal(http.ListenAndServeTLS(":8443", "/certs/server.pem", "/certs/server.key", mux))
}

func (s *server) hook(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	key := r.Header.Get("Idempotency-Key")
	if err != nil || key == "" {
		s.mu.Lock()
		if key == "" {
			s.noKey++
		} else {
			s.badReq++
		}
		s.mu.Unlock()
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	valid, skew := s.verify(r.Header.Get("X-Gateway-Signature"), body, now)
	var payload struct {
		EventType string `json:"event_type"`
		Data      struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &payload)
	sum := sha256.Sum256(body)

	mode := r.PathValue("mode")
	s.mu.Lock()
	n := len(s.byKey[key]) + 1
	status := http.StatusOK
	switch mode {
	case "dead":
		status = http.StatusInternalServerError
	case "flaky":
		if s.fails(key, n) {
			status = http.StatusInternalServerError
		}
	case "ok":
	default:
		status = http.StatusNotFound
	}
	s.byKey[key] = append(s.byKey[key], attempt{
		At: now, Mode: mode, Status: status, SigValid: valid, SigSkewSec: skew,
		BodySHA256: hex.EncodeToString(sum[:]), EventType: payload.EventType, IntentID: payload.Data.ID,
	})
	s.mu.Unlock()
	w.WriteHeader(status)
}

// fails decides deterministically from (seed, key, attempt) so a run can be repeated.
func (s *server) fails(key string, n int) bool {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", s.seed, key, n)))
	return float64(binary.BigEndian.Uint64(h[:8]))/float64(^uint64(0)) < s.failRate
}

func (s *server) verify(header string, body []byte, now time.Time) (bool, int64) {
	var ts, sig string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			sig = v
		}
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || sig == "" {
		return false, 0
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(ts + "." + string(body)))
	return hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(sig)), now.Unix() - t
}

func (s *server) stats(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type keyInfo struct {
		Key      string    `json:"key"`
		Attempts []attempt `json:"attempts"`
	}
	out := map[string]any{}
	total, sigValid, sigInvalid, delivered := 0, 0, 0, 0
	attemptsPerKey := map[int]int{}
	keysBodyChanged := 0
	keysPerIntent := map[int64]map[string]bool{}
	byMode := map[string]int{}
	var maxSkew int64
	var keys []keyInfo
	for k, as := range s.byKey {
		attemptsPerKey[len(as)]++
		bodies := map[string]bool{}
		ok := false
		for _, a := range as {
			total++
			byMode[a.Mode]++
			if a.SigValid {
				sigValid++
			} else {
				sigInvalid++
			}
			if a.SigSkewSec > maxSkew {
				maxSkew = a.SigSkewSec
			}
			bodies[a.BodySHA256] = true
			if a.Status == http.StatusOK {
				ok = true
			}
			if keysPerIntent[a.IntentID] == nil {
				keysPerIntent[a.IntentID] = map[string]bool{}
			}
			keysPerIntent[a.IntentID][k] = true
		}
		if ok {
			delivered++
		}
		if len(bodies) > 1 {
			keysBodyChanged++
		}
		keys = append(keys, keyInfo{Key: k, Attempts: as})
	}
	intentsWithManyKeys := 0
	for _, ks := range keysPerIntent {
		if len(ks) > 1 {
			intentsWithManyKeys++
		}
	}
	out["requests_total"] = total
	out["requests_by_mode"] = byMode
	out["signature_valid"] = sigValid
	out["signature_invalid"] = sigInvalid
	out["max_signature_age_s"] = maxSkew
	out["distinct_idempotency_keys"] = len(s.byKey)
	out["keys_with_a_200"] = delivered
	out["keys_whose_body_changed_between_attempts"] = keysBodyChanged
	out["intents_seen_under_more_than_one_key"] = intentsWithManyKeys
	out["attempts_per_key_histogram"] = attemptsPerKey
	out["requests_without_idempotency_key"] = s.noKey
	out["fail_rate"] = s.failRate
	if r.URL.Query().Get("full") == "1" {
		sort.Slice(keys, func(i, j int) bool { return keys[i].Key < keys[j].Key })
		out["keys"] = keys
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *server) reset(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.byKey = map[string][]attempt{}
	s.noKey, s.badReq = 0, 0
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
