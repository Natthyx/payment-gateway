package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Server
	Env          string // "live" or "test" — drives API-key namespacing
	Port         string
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// Logging: trace|debug|info|warn|error
	LogLevel string

	// PostgreSQL
	DatabaseURL           string // runtime connection (least-privilege role in docker-compose)
	MigrationsDatabaseURL string // table owner used only for migrations; defaults to DatabaseURL
	MigrationsPath        string // file path for golang-migrate

	// Redis
	RedisAddr     string
	RedisPassword string
	RedisDB       int

	// Stripe
	StripeSecretKey     string
	StripeWebhookSecret string

	// Chapa
	ChapaSecretKey string

	// Telebirr
	TelebirrAppID     string
	TelebirrAppKey    string
	TelebirrPublicKey string
	TelebirrShortCode string

	// Gateway
	BaseCallbackURL string  // e.g. "https://yourdomain.com"
	FeePercent      float64 // 0.029 = 2.9%

	// Limits
	RateLimitPerMerchantPerMinute int
	IPRateLimitPerMinute          int
	// TrustedProxies lists proxy networks (CIDRs or IPs) whose
	// X-Forwarded-For header is believed. Empty: the header is ignored and
	// the per-IP limit uses the TCP peer address.
	TrustedProxies []string
	// AuthCacheTTL caches successful API-key checks in-process so bcrypt does
	// not run on every request. A rotated key keeps working on other
	// instances for at most this long. 0 disables the cache.
	AuthCacheTTL                 time.Duration
	OutboundWebhookSigningSecret string // HMAC secret for merchant-facing webhook signatures

	// Providers — "real" (default) or "mock". Mock is for load testing only.
	ProviderMode string
	MockLatency  time.Duration // artificial latency per mock provider call
	MockTxStatus string        // status the mock reports from GetTransaction
}

func Load() (*Config, error) {
	cfg := &Config{
		Env:                           strings.ToLower(getEnv("ENV", "live")),
		Port:                          getEnv("PORT", "8080"),
		ReadTimeout:                   getDuration("READ_TIMEOUT", 10*time.Second),
		WriteTimeout:                  getDuration("WRITE_TIMEOUT", 30*time.Second),
		LogLevel:                      strings.ToLower(getEnv("LOG_LEVEL", "info")),
		DatabaseURL:                   mustGetEnv("DATABASE_URL"),
		MigrationsPath:                getEnv("MIGRATIONS_PATH", "file://migrations"),
		RedisAddr:                     getEnv("REDIS_ADDR", "localhost:6379"),
		RedisPassword:                 getEnv("REDIS_PASSWORD", ""),
		RedisDB:                       getInt("REDIS_DB", 0),
		StripeSecretKey:               getEnv("STRIPE_SECRET_KEY", ""),
		StripeWebhookSecret:           getEnv("STRIPE_WEBHOOK_SECRET", ""),
		ChapaSecretKey:                getEnv("CHAPA_SECRET_KEY", ""),
		TelebirrAppID:                 getEnv("TELEBIRR_APP_ID", ""),
		TelebirrAppKey:                getEnv("TELEBIRR_APP_KEY", ""),
		TelebirrPublicKey:             getEnv("TELEBIRR_PUBLIC_KEY", ""),
		TelebirrShortCode:             getEnv("TELEBIRR_SHORT_CODE", ""),
		BaseCallbackURL:               mustGetEnv("BASE_CALLBACK_URL"),
		FeePercent:                    getFloat("FEE_PERCENT", 0.029),
		RateLimitPerMerchantPerMinute: getInt("RATE_LIMIT_PER_MERCHANT", 600),
		IPRateLimitPerMinute:          getInt("IP_RATE_LIMIT_PER_MINUTE", 200),
		TrustedProxies:                strings.Split(getEnv("TRUSTED_PROXIES", ""), ","),
		AuthCacheTTL:                  getDuration("AUTH_CACHE_TTL", 30*time.Second),
		OutboundWebhookSigningSecret:  mustGetEnv("OUTBOUND_WEBHOOK_SIGNING_SECRET"),
		ProviderMode:                  strings.ToLower(getEnv("PROVIDER_MODE", "real")),
		MockLatency:                   getDuration("MOCK_PROVIDER_LATENCY", 0),
		MockTxStatus:                  getEnv("MOCK_PROVIDER_TX_STATUS", "pending"),
	}
	cfg.MigrationsDatabaseURL = getEnv("MIGRATIONS_DATABASE_URL", cfg.DatabaseURL)
	if cfg.Env != "live" && cfg.Env != "test" {
		return nil, fmt.Errorf("ENV must be 'live' or 'test', got %q", cfg.Env)
	}
	if cfg.FeePercent < 0 || cfg.FeePercent >= 1 {
		return nil, fmt.Errorf("FEE_PERCENT must be in [0,1), got %v", cfg.FeePercent)
	}
	if cfg.ProviderMode != "real" && cfg.ProviderMode != "mock" {
		return nil, fmt.Errorf("PROVIDER_MODE must be 'real' or 'mock', got %q", cfg.ProviderMode)
	}
	if cfg.ProviderMode == "mock" && cfg.Env == "live" {
		return nil, fmt.Errorf("PROVIDER_MODE=mock is not allowed with ENV=live")
	}
	return cfg, nil
}

// CallbackURL builds the full webhook URL for a given provider.
func (c *Config) CallbackURL(provider string) string {
	return fmt.Sprintf("%s/webhooks/%s", strings.TrimRight(c.BaseCallbackURL, "/"), provider)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func mustGetEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic(fmt.Sprintf("required environment variable %q is not set", key))
	}
	return v
}

func getInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func getDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
