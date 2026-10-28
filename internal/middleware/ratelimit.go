package middleware

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"

	"github.com/Natthyx/payment-gateway/internal/apierr"
	"github.com/Natthyx/payment-gateway/internal/httpx"
)

// PerMerchantRateLimiter applies a per-merchant sliding-window budget, stored
// in Redis so it holds across pods. Must be installed AFTER
// AuthenticateMerchant so the merchant ID is on the context.
//
// The estimate is prev_minute_count × (unelapsed share of the window) +
// this_minute_count. A plain fixed window would let 2× the budget through
// across a minute boundary. A merchant over budget gets HTTP 429 with
// Retry-After. We fail-open if Redis is unreachable because a payment
// gateway is more useful overcounting requests than down.
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
		now := time.Now()
		windowStart := now.Truncate(time.Minute)
		curKey := fmt.Sprintf("ratelimit:merchant:%d:%d", m.ID, windowStart.Unix())
		prevKey := fmt.Sprintf("ratelimit:merchant:%d:%d", m.ID, windowStart.Add(-time.Minute).Unix())

		ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
		defer cancel()

		pipe := l.redis.Pipeline()
		incr := pipe.Incr(ctx, curKey)
		pipe.Expire(ctx, curKey, 2*time.Minute+5*time.Second)
		prev := pipe.Get(ctx, prevKey)
		if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
			log.Warn().Err(err).Msg("rate-limit redis error — failing open")
			next.ServeHTTP(w, r)
			return
		}
		prevCount, _ := prev.Int64() // redis.Nil → 0
		weight := 1 - float64(now.Sub(windowStart))/float64(time.Minute)
		estimate := float64(prevCount)*weight + float64(incr.Val())

		remaining := l.perMinute - int(math.Ceil(estimate))
		if remaining < 0 {
			remaining = 0
		}
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(l.perMinute))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(windowStart.Unix()+60, 10))

		if estimate > float64(l.perMinute) {
			w.Header().Set("Retry-After", "60")
			httpx.Error(w, r, apierr.RateLimited("per-merchant rate limit exceeded"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// IPRateLimitExceeded is the 429 body for the per-IP limiter, in the same
// envelope as every other API error.
func IPRateLimitExceeded(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "60")
	httpx.Error(w, r, apierr.RateLimited("per-IP rate limit exceeded"))
}
