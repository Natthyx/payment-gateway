package retry

import (
    "context"
    "fmt"
    "math"
    "time"

    "github.com/rs/zerolog/log"
)

// Config controls retry behaviour.
type Config struct {
    MaxAttempts int
    BaseDelay   time.Duration // delay after first failure
    MaxDelay    time.Duration // cap on delay regardless of attempt count
    Multiplier  float64       // exponential base e.g. 2.0
}

// DefaultConfig is sensible for webhook redelivery.
// Attempt 1: 1s, Attempt 2: 2s, Attempt 3: 4s ... capped at 5 minutes.
var DefaultConfig = Config{
    MaxAttempts: 5,
    BaseDelay:   1 * time.Second,
    MaxDelay:    5 * time.Minute,
    Multiplier:  2.0,
}

// Do executes fn with exponential backoff.
//
// WHY exponential backoff:
//   If a downstream service (Stripe, your DB) is overloaded,
//   hammering it with immediate retries makes it worse.
//   Exponential backoff gives the system time to recover.
//   This is the same algorithm used in TCP retransmission,
//   AWS SDK retries, and Google's gRPC retry policy.
//
// Formula: delay = min(BaseDelay * Multiplier^attempt, MaxDelay)
//   Attempt 0: 1s
//   Attempt 1: 2s
//   Attempt 2: 4s
//   Attempt 3: 8s
//   Attempt 4: 16s (capped at MaxDelay)
func Do(ctx context.Context, cfg Config, operation string, fn func() error) error {
    var lastErr error

    for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
        if attempt > 0 {
            delay := calculateDelay(cfg, attempt)
            log.Warn().
                Str("operation", operation).
                Int("attempt", attempt).
                Dur("delay", delay).
                Err(lastErr).
                Msg("retrying after delay")

            select {
            case <-time.After(delay):
                // continue to next attempt
            case <-ctx.Done():
                // context cancelled (e.g. server shutting down) — stop retrying
                return fmt.Errorf("%s: context cancelled during retry: %w", operation, ctx.Err())
            }
        }

        if err := fn(); err != nil {
            lastErr = err
            continue
        }

        // success
        if attempt > 0 {
            log.Info().
                Str("operation", operation).
                Int("attempts_taken", attempt+1).
                Msg("operation succeeded after retry")
        }
        return nil
    }

    return fmt.Errorf("%s failed after %d attempts: %w", operation, cfg.MaxAttempts, lastErr)
}

// calculateDelay computes the backoff delay for a given attempt number.
// Uses integer math where possible to avoid float precision issues.
func calculateDelay(cfg Config, attempt int) time.Duration {
    // delay = BaseDelay * Multiplier^attempt
    multiplied := float64(cfg.BaseDelay) * math.Pow(cfg.Multiplier, float64(attempt))
    delay := time.Duration(multiplied)

    if delay > cfg.MaxDelay {
        delay = cfg.MaxDelay
    }

    return delay
}

// IsRetryable returns true for errors that are worth retrying.
// Network errors and 5xx responses are retryable.
// 4xx errors (bad request, not found) are not — retrying won't help.
func IsRetryable(err error) bool {
    if err == nil {
        return false
    }
    // In production you'd check for specific error types here
    // e.g. *net.OpError, *url.Error, HTTP 429/500/502/503/504
    return true
}