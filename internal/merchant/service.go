package merchant

import (
    "context"
    "crypto/rand"
    "crypto/sha256"
    "encoding/hex"
    "errors"
    "fmt"
    "strings"
    "sync"
    "time"

    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgxpool"
    "golang.org/x/crypto/bcrypt"

    "github.com/Natthyx/payment-gateway/internal/models"
)

// API key format: gw_<env>_<lookup_16hex>_<secret_48hex>
//
//   gw_       – product prefix; spot-check in logs/dashboards
//   live|test – environment selector for blast-radius separation
//   lookup    – 8 random bytes hex-encoded → 16 chars; plaintext, indexed
//   secret    – 24 random bytes hex-encoded → 48 chars; bcrypted, never stored plaintext
//
// Authenticate does a single index lookup by `lookup`, then one bcrypt compare
// against the secret. O(1) at any merchant count. Compromise of api_key_lookup
// alone is harmless; an attacker still needs the secret half to authenticate.
const (
    apiKeyEnvLive   = "live"
    apiKeyEnvTest   = "test"
    apiKeyPrefix    = "gw"
    apiKeyParts     = 4 // ["gw", env, lookup, secret]
    apiKeyLookupLen = 16
    apiKeySecretLen = 48
    bcryptCost      = 12
)

var (
    ErrInvalidAPIKey       = errors.New("invalid API key")
    ErrEmailAlreadyTaken   = errors.New("email already registered")
    ErrMerchantNotFound    = errors.New("merchant not found")
    ErrMerchantInactive    = errors.New("merchant is inactive")
    ErrMissingRequired     = errors.New("name and email are required")
)

type Service struct {
    db  *pgxpool.Pool
    env string // "live" or "test"

    // Successful authentications, keyed by SHA-256 of the full API key, so a
    // bcrypt compare (~230 ms at cost 12) is not paid on every request.
    // Disabled when authCacheTTL is 0.
    authCacheTTL time.Duration
    authMu       sync.Mutex
    authCache    map[[32]byte]authCacheEntry
}

type authCacheEntry struct {
    merchant models.Merchant
    expires  time.Time
}

// authCacheMax bounds memory; the cache is simply cleared when it is full.
const authCacheMax = 10000

func NewService(db *pgxpool.Pool, env string) *Service {
    if env != apiKeyEnvLive && env != apiKeyEnvTest {
        env = apiKeyEnvLive
    }
    return &Service{db: db, env: env, authCache: map[[32]byte]authCacheEntry{}}
}

// SetAuthCacheTTL enables (ttl > 0) or disables the authentication cache.
// A rotated key stays valid on other instances for at most ttl.
func (s *Service) SetAuthCacheTTL(ttl time.Duration) {
    s.authMu.Lock()
    s.authCacheTTL = ttl
    s.authCache = map[[32]byte]authCacheEntry{}
    s.authMu.Unlock()
}

// Create onboards a new merchant and returns their API key.
// The key is returned ONCE and never reconstructible afterwards.
func (s *Service) Create(ctx context.Context, req models.CreateMerchantRequest) (*models.CreateMerchantResponse, error) {
    if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Email) == "" {
        return nil, ErrMissingRequired
    }

    key, lookup, hash, err := s.mintKey()
    if err != nil {
        return nil, err
    }

    var m models.Merchant
    err = s.db.QueryRow(ctx, `
        INSERT INTO merchants (name, email, api_key_hash, api_key_lookup)
        VALUES ($1, LOWER($2), $3, $4)
        RETURNING id, name, email, is_active, created_at, updated_at
    `, req.Name, req.Email, hash, lookup).Scan(
        &m.ID, &m.Name, &m.Email, &m.IsActive, &m.CreatedAt, &m.UpdatedAt,
    )
    if err != nil {
        if isUniqueViolation(err) {
            return nil, ErrEmailAlreadyTaken
        }
        return nil, fmt.Errorf("inserting merchant: %w", err)
    }

    return &models.CreateMerchantResponse{Merchant: &m, APIKey: key}, nil
}

// Authenticate validates a presented API key in O(1) DB calls.
// 1) Parse key, extract lookup
// 2) Single indexed SELECT by lookup
// 3) Single bcrypt.CompareHashAndPassword on the secret half
func (s *Service) Authenticate(ctx context.Context, apiKey string) (*models.Merchant, error) {
    cacheKey := sha256.Sum256([]byte(apiKey))
    s.authMu.Lock()
    if e, ok := s.authCache[cacheKey]; ok && s.authCacheTTL > 0 && time.Now().Before(e.expires) {
        m := e.merchant
        s.authMu.Unlock()
        return &m, nil
    }
    s.authMu.Unlock()

    env, lookup, secret, err := parseKey(apiKey)
    if err != nil {
        return nil, ErrInvalidAPIKey
    }
    if env != s.env {
        return nil, ErrInvalidAPIKey
    }

    var m models.Merchant
    err = s.db.QueryRow(ctx, `
        SELECT id, name, email, api_key_hash, api_key_lookup,
               is_active, created_at, updated_at
        FROM merchants
        WHERE api_key_lookup = $1
    `, lookup).Scan(
        &m.ID, &m.Name, &m.Email, &m.APIKeyHash, &m.APIKeyLookup,
        &m.IsActive, &m.CreatedAt, &m.UpdatedAt,
    )
    if errors.Is(err, pgx.ErrNoRows) {
        return nil, ErrInvalidAPIKey
    }
    if err != nil {
        return nil, fmt.Errorf("looking up merchant: %w", err)
    }
    if !m.IsActive {
        return nil, ErrMerchantInactive
    }

    // Constant-time compare; bcrypt itself is timing-safe.
    if err := bcrypt.CompareHashAndPassword([]byte(m.APIKeyHash), []byte(secret)); err != nil {
        return nil, ErrInvalidAPIKey
    }

    m.APIKeyHash = ""
    m.APIKeyLookup = ""

    s.authMu.Lock()
    if s.authCacheTTL > 0 {
        if len(s.authCache) >= authCacheMax {
            s.authCache = map[[32]byte]authCacheEntry{}
        }
        s.authCache[cacheKey] = authCacheEntry{merchant: m, expires: time.Now().Add(s.authCacheTTL)}
    }
    s.authMu.Unlock()
    return &m, nil
}

// forgetMerchant drops cached authentications of one merchant (this
// instance only; other instances expire theirs within the TTL).
func (s *Service) forgetMerchant(merchantID int64) {
    s.authMu.Lock()
    for k, e := range s.authCache {
        if e.merchant.ID == merchantID {
            delete(s.authCache, k)
        }
    }
    s.authMu.Unlock()
}

// GetByID fetches a merchant by ID.
func (s *Service) GetByID(ctx context.Context, id int64) (*models.Merchant, error) {
    var m models.Merchant
    err := s.db.QueryRow(ctx, `
        SELECT id, name, email, is_active, created_at, updated_at
        FROM merchants WHERE id = $1
    `, id).Scan(&m.ID, &m.Name, &m.Email, &m.IsActive, &m.CreatedAt, &m.UpdatedAt)
    if errors.Is(err, pgx.ErrNoRows) {
        return nil, ErrMerchantNotFound
    }
    if err != nil {
        return nil, fmt.Errorf("fetching merchant: %w", err)
    }
    return &m, nil
}

// RegenerateKey rotates a merchant's API key. Old key is invalidated atomically.
func (s *Service) RegenerateKey(ctx context.Context, merchantID int64) (string, error) {
    key, lookup, hash, err := s.mintKey()
    if err != nil {
        return "", err
    }
    result, err := s.db.Exec(ctx, `
        UPDATE merchants SET api_key_hash = $1, api_key_lookup = $2 WHERE id = $3
    `, hash, lookup, merchantID)
    if err != nil {
        return "", fmt.Errorf("rotating api key: %w", err)
    }
    if result.RowsAffected() == 0 {
        return "", ErrMerchantNotFound
    }
    s.forgetMerchant(merchantID)
    return key, nil
}

func (s *Service) mintKey() (apiKey, lookup, hash string, err error) {
    lookup, err = randomHex(8) // 16 chars
    if err != nil {
        return "", "", "", fmt.Errorf("generating lookup: %w", err)
    }
    secret, err := randomHex(24) // 48 chars
    if err != nil {
        return "", "", "", fmt.Errorf("generating secret: %w", err)
    }
    h, err := bcrypt.GenerateFromPassword([]byte(secret), bcryptCost)
    if err != nil {
        return "", "", "", fmt.Errorf("hashing secret: %w", err)
    }
    apiKey = fmt.Sprintf("%s_%s_%s_%s", apiKeyPrefix, s.env, lookup, secret)
    return apiKey, lookup, string(h), nil
}

// ParseKey is exported for use in tests. Returns (env, lookup, secret).
func ParseKey(k string) (env, lookup, secret string, err error) {
    return parseKey(k)
}

func parseKey(k string) (env, lookup, secret string, err error) {
    parts := strings.Split(k, "_")
    if len(parts) != apiKeyParts || parts[0] != apiKeyPrefix {
        return "", "", "", ErrInvalidAPIKey
    }
    env = parts[1]
    if env != apiKeyEnvLive && env != apiKeyEnvTest {
        return "", "", "", ErrInvalidAPIKey
    }
    lookup = parts[2]
    secret = parts[3]
    if len(lookup) != apiKeyLookupLen || len(secret) != apiKeySecretLen {
        return "", "", "", ErrInvalidAPIKey
    }
    if !isHex(lookup) || !isHex(secret) {
        return "", "", "", ErrInvalidAPIKey
    }
    return env, lookup, secret, nil
}

func randomHex(n int) (string, error) {
    b := make([]byte, n)
    if _, err := rand.Read(b); err != nil {
        return "", err
    }
    return hex.EncodeToString(b), nil
}

func isHex(s string) bool {
    _, err := hex.DecodeString(s)
    return err == nil
}

func isUniqueViolation(err error) bool {
    if err == nil {
        return false
    }
    msg := err.Error()
    return strings.Contains(msg, "23505") || strings.Contains(msg, "duplicate key")
}
