package retry

import (
    "context"
    "errors"
    "testing"
    "time"
)

func TestDo_SucceedsAfterRetries(t *testing.T) {
    cfg := Config{MaxAttempts: 3, BaseDelay: 1 * time.Millisecond, MaxDelay: 5 * time.Millisecond, Multiplier: 2.0}
    attempts := 0
    err := Do(context.Background(), cfg, "test", func() error {
        attempts++
        if attempts < 3 {
            return errors.New("transient")
        }
        return nil
    })
    if err != nil {
        t.Fatalf("expected success, got %v", err)
    }
    if attempts != 3 {
        t.Fatalf("expected 3 attempts, got %d", attempts)
    }
}

func TestDo_FailsAfterMax(t *testing.T) {
    cfg := Config{MaxAttempts: 2, BaseDelay: 1 * time.Millisecond, MaxDelay: 5 * time.Millisecond, Multiplier: 2.0}
    err := Do(context.Background(), cfg, "test", func() error { return errors.New("perm") })
    if err == nil {
        t.Fatal("expected error")
    }
}

func TestCalculateDelay(t *testing.T) {
    cfg := Config{BaseDelay: time.Second, MaxDelay: 10 * time.Second, Multiplier: 2.0}
    got := calculateDelay(cfg, 5)
    if got > cfg.MaxDelay {
        t.Fatalf("delay %v exceeded MaxDelay %v", got, cfg.MaxDelay)
    }
    if calculateDelay(cfg, 1) != 2*time.Second {
        t.Fatalf("attempt=1 expected 2s")
    }
}

func TestDo_RespectsContext(t *testing.T) {
    cfg := Config{MaxAttempts: 5, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second, Multiplier: 2.0}
    ctx, cancel := context.WithCancel(context.Background())
    cancel()
    err := Do(ctx, cfg, "test", func() error { return errors.New("e") })
    if err == nil {
        t.Fatal("expected ctx err")
    }
}
