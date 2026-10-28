package middleware

import (
    "context"
    "fmt"
    "net/http"
    "strconv"
    "time"

    "github.com/redis/go-redis/v9"
    "github.com/rs/zerolog/log"
)

// PerMerchantRateLimiter applies a fixed-window per-merchant token budget,
// stored in Redis so it survives across pods. Must be installed AFTER
// AuthenticateMerchant so the merchant ID is on the context.
//
// The window is 60s; perMinute is the budget. A merchant exceeding the budget
// gets HTTP 429 with Retry-After. We fail-open if Redis is unreachable
// because a payment-gateway is more useful overcounting requests than down.
type PerMerchantRateLimiter struct {
    redis     *redis.Client
    perMinute int
}

func NewPerMerchantRateLimiter(r *redis.Client, perMinute int) *PerMerchantRateLimiter {
    if perMinute <= 0 {
        perMinute = 600
    }
    return &PerMerchantRateLimiter{redis: r, perMinute: perMinute}
}

func (l *PerMerchantRateLimiter) Handler(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        m, ok := GetMerchant(r.Context())
        if !ok {
            next.ServeHTTP(w, r)
            return
        }
        windowStart := time.Now().Truncate(time.Minute).Unix()
        key := fmt.Sprintf("ratelimit:merchant:%d:%d", m.ID, windowStart)

        ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
        defer cancel()

        count, err := l.redis.Incr(ctx, key).Result()
        if err != nil {
            log.Warn().Err(err).Msg("rate-limit redis error — failing open")
            next.ServeHTTP(w, r)
            return
        }
        if count == 1 {
            l.redis.Expire(ctx, key, 65*time.Second)
        }
        remaining := l.perMinute - int(count)
        if remaining < 0 {
            remaining = 0
        }
        w.Header().Set("X-RateLimit-Limit", strconv.Itoa(l.perMinute))
        w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
        w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(windowStart+60, 10))

        if int(count) > l.perMinute {
            w.Header().Set("Retry-After", "60")
            w.Header().Set("Content-Type", "application/json")
            w.WriteHeader(http.StatusTooManyRequests)
            _, _ = w.Write([]byte(`{"success":false,"error":{"code":"rate_limit_exceeded","message":"per-merchant rate limit exceeded"}}`))
            return
        }
        next.ServeHTTP(w, r)
    })
}
