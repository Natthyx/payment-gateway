package db

import (
    "context"
    "encoding/json"
    "fmt"
    "time"

    "github.com/redis/go-redis/v9"
)

const idempotencyTTL = 24 * time.Hour

// IdempotencyStore manages idempotency keys in Redis.
//
// HOW IT WORKS:
//   1. Request arrives with Idempotency-Key header
//   2. We check Redis: does this key exist for this merchant?
//   3. If YES  → return the cached response immediately (replay)
//   4. If NO   → process the request, store the result, return it
//
// This guarantees that no matter how many times a client retries
// (due to network timeouts, crashes, etc.), the payment is only
// charged once. Same guarantee Stripe provides to its customers.
type IdempotencyStore struct {
    redis *redis.Client
}

func NewIdempotencyStore(redis *redis.Client) *IdempotencyStore {
    return &IdempotencyStore{redis: redis}
}

// CachedResponse holds the stored result of a previous request.
type CachedResponse struct {
    StatusCode  int             `json:"status_code"`
    Body        json.RawMessage `json:"body"`
    Fingerprint string          `json:"fingerprint,omitempty"` // hash of the request that produced Body
}

// Get retrieves a cached response for a given merchant + idempotency key.
// Returns nil if no cached response exists (first request).
func (s *IdempotencyStore) Get(ctx context.Context, merchantID int64, key string) (*CachedResponse, error) {
    redisKey := fmt.Sprintf("idempotency:%d:%s", merchantID, key)

    val, err := s.redis.Get(ctx, redisKey).Result()
    if err == redis.Nil {
        return nil, nil // cache miss — first time seeing this key
    }
    if err != nil {
        return nil, fmt.Errorf("redis get idempotency key: %w", err)
    }

    var cached CachedResponse
    if err := json.Unmarshal([]byte(val), &cached); err != nil {
        return nil, fmt.Errorf("unmarshalling cached response: %w", err)
    }

    return &cached, nil
}

// Set stores a response for a given merchant + idempotency key.
// TTL is 24 hours — after that, the key expires and a fresh request is allowed.
func (s *IdempotencyStore) Set(ctx context.Context, merchantID int64, key string, resp CachedResponse) error {
    redisKey := fmt.Sprintf("idempotency:%d:%s", merchantID, key)

    data, err := json.Marshal(resp)
    if err != nil {
        return fmt.Errorf("marshalling cached response: %w", err)
    }

    if err := s.redis.Set(ctx, redisKey, data, idempotencyTTL).Err(); err != nil {
        return fmt.Errorf("redis set idempotency key: %w", err)
    }

    return nil
}