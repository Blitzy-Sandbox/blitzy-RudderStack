// Package profiles implements the Profiles REST API and associated caching
// layer for the Identity Resolution feature (E-027). This package provides
// read-through Redis caching of identity profile data, enabling sub-200ms
// response times for the Profiles API endpoints.
//
// Cache Architecture:
//
//  1. GetProfile checks Redis for a cached profile
//  2. On miss, the caller fetches from the identity graph via storage
//  3. SetProfile populates the cache for future requests
//  4. InvalidateProfile removes stale data when the identity graph is updated
//
// Thread Safety:
//
//	All cache operations are safe for concurrent use. The underlying Redis client
//	manages its own connection pool (go-redis/v9), and no additional synchronization
//	is required.
package profiles

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"

	"github.com/rudderlabs/rudder-server/identity/storage"
)

// pkgLogger is the package-level structured logger for the identity/profiles package.
// Follows the exact pattern from warehouse/identity/identity.go:30-34.
var pkgLogger logger.Logger

func init() {
	pkgLogger = logger.NewLogger().Child("identity").Child("profiles")
}

const (
	// cacheKeyPrefix is the namespace prefix for profile cache keys in Redis.
	// Key format: "profile:{segmentID}" — e.g., "profile:123".
	// The prefix provides namespace isolation to avoid collisions with other
	// Redis keys in the same database.
	cacheKeyPrefix = "profile:"

	// defaultCacheTTL is the fallback cache TTL used when no configuration is
	// provided. Set to 5 minutes as a balance between data freshness and cache
	// hit rates for the Profiles API (E-027).
	defaultCacheTTL = 5 * time.Minute
)

// cacheKey generates the deterministic Redis cache key for a profile segment ID.
// Uses strconv.FormatInt for efficient int64-to-string conversion.
// Format: "profile:{segmentID}" — e.g., cacheKey(42) returns "profile:42".
func cacheKey(segmentID int64) string {
	return cacheKeyPrefix + strconv.FormatInt(segmentID, 10)
}

// ProfileCache defines the caching contract for profile data.
// Implementations must be safe for concurrent use from multiple HTTP handler
// goroutines. The interface is designed for read-through caching:
//
//  1. GetProfile checks the cache
//  2. On miss, the caller fetches from the identity graph
//  3. SetProfile populates the cache for future requests
//  4. InvalidateProfile removes stale data on profile updates
//
// Error handling convention:
//   - GetProfile returns (nil, nil) on cache miss (key not found)
//   - GetProfile returns (nil, error) on Redis failure or deserialization error
//   - SetProfile and InvalidateProfile return errors that callers may log and ignore
type ProfileCache interface {
	// GetProfile retrieves a cached profile by segment ID.
	// Returns (nil, nil) on cache miss, (*ProfileData, nil) on hit,
	// or (nil, error) on Redis failure or deserialization error.
	GetProfile(ctx context.Context, segmentID int64) (*storage.ProfileData, error)

	// SetProfile caches a profile with the given TTL.
	// A TTL of 0 means use the configured default TTL.
	// Passing nil data is a no-op (returns nil immediately).
	SetProfile(ctx context.Context, segmentID int64, data *storage.ProfileData, ttl time.Duration) error

	// InvalidateProfile removes a cached profile from Redis.
	// This must be called whenever the identity graph is updated for a segment,
	// to prevent stale data from being served by the Profiles API.
	InvalidateProfile(ctx context.Context, segmentID int64) error
}

// RedisClient defines the minimal Redis interface used by the profile cache.
// This interface is satisfied by both *redis.Client (standalone mode) and
// *redis.ClusterClient (cluster mode) from go-redis/v9, enabling deployment
// flexibility without code changes.
//
// In production, the concrete client is created from configuration and injected.
// In tests, a mock implementation satisfying this interface is used.
type RedisClient interface {
	// Get retrieves a string value by key. Returns *redis.StringCmd whose
	// .Result() returns (string, error). redis.Nil error indicates cache miss.
	Get(ctx context.Context, key string) *redis.StringCmd

	// Set stores a key-value pair with an expiration TTL.
	// value is serialized to string before storage.
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd

	// Del removes one or more keys. Returns *redis.IntCmd whose
	// .Result() returns the number of keys removed.
	Del(ctx context.Context, keys ...string) *redis.IntCmd

	// Ping checks connectivity to the Redis server.
	Ping(ctx context.Context) *redis.StatusCmd
}

// RedisProfileCache implements ProfileCache using Redis (go-redis/v9).
// It serializes/deserializes storage.ProfileData as JSON for Redis SET/GET
// operations using the jsonrs library (required by the project's linter rules).
//
// Thread-safe: Redis operations are inherently connection-pooled and
// concurrent-safe via go-redis/v9. No additional synchronization is needed.
type RedisProfileCache struct {
	client     RedisClient
	defaultTTL time.Duration
	logger     logger.Logger
}

// NewRedisProfileCache creates a new Redis-backed profile cache.
//
// Parameters:
//   - client: Redis client (standalone or cluster). If nil, returns a NoopCache
//     for graceful degradation — the Profiles API will still work, just without
//     caching.
//   - conf: Configuration for cache TTL under "Identity.Profiles.Cache.TTL".
//     If nil, defaults are used.
//   - log: Structured logger. If nil, uses the package-level pkgLogger.
//
// Configuration keys (via rudder-go-kit/config):
//   - Identity.Profiles.Cache.TTL: Cache TTL in seconds (default: 300 = 5 minutes)
//
// Returns:
//   - A ProfileCache implementation (RedisProfileCache or NoopCache).
func NewRedisProfileCache(client RedisClient, conf *config.Config, log logger.Logger) ProfileCache {
	if client == nil {
		if log != nil {
			log.Infon("Redis client is nil, using no-op cache")
		} else {
			pkgLogger.Infon("Redis client is nil, using no-op cache")
		}
		return &NoopCache{}
	}

	if log == nil {
		log = pkgLogger
	}

	cacheTTL := defaultCacheTTL
	if conf != nil {
		cacheTTL = time.Duration(conf.GetInt("Identity.Profiles.Cache.TTL", 300)) * time.Second
	}

	return &RedisProfileCache{
		client:     client,
		defaultTTL: cacheTTL,
		logger:     log.Child("cache"),
	}
}

// GetProfile retrieves a cached profile from Redis by segment ID.
//
// Returns:
//   - (*storage.ProfileData, nil) on cache hit — the deserialized profile data.
//   - (nil, nil) on cache miss (redis.Nil error) — not an error, just absent.
//   - (nil, error) on Redis failure or JSON deserialization error.
//
// On deserialization failure, the corrupted cache entry is automatically deleted
// to prevent repeatedly serving bad data.
func (c *RedisProfileCache) GetProfile(ctx context.Context, segmentID int64) (*storage.ProfileData, error) {
	key := cacheKey(segmentID)

	val, err := c.client.Get(ctx, key).Result()
	if err != nil {
		if err == redis.Nil {
			// Cache miss — not an error condition, the profile is simply not cached.
			return nil, nil //nolint:nilnil // nil,nil signals cache miss per ProfileCache contract
		}
		return nil, fmt.Errorf("redis get profile %d: %w", segmentID, err)
	}

	// Deserialize JSON using jsonrs (mandated by project linter rules).
	var profileData storage.ProfileData
	if err := jsonrs.Unmarshal([]byte(val), &profileData); err != nil {
		c.logger.Errorn("Error deserializing cached profile",
			logger.NewIntField("segmentID", segmentID),
			obskit.Error(err),
		)
		// Delete the corrupted cache entry to prevent repeated failures.
		_ = c.client.Del(ctx, key).Err()
		return nil, fmt.Errorf("deserialize cached profile %d: %w", segmentID, err)
	}

	c.logger.Debugn("Cache hit for profile",
		logger.NewIntField("segmentID", segmentID),
	)

	return &profileData, nil
}

// SetProfile caches a profile in Redis with JSON serialization.
// If ttl is 0, the configured default TTL is used. If data is nil, the
// operation is a no-op (returns nil immediately — nothing to cache).
//
// The profile data is serialized to JSON using jsonrs and stored as a string
// in Redis with the specified TTL for automatic expiration.
func (c *RedisProfileCache) SetProfile(ctx context.Context, segmentID int64, data *storage.ProfileData, ttl time.Duration) error {
	if data == nil {
		return nil // Nothing to cache
	}

	key := cacheKey(segmentID)

	// Use the configured default TTL when the caller passes zero.
	if ttl == 0 {
		ttl = c.defaultTTL
	}

	// Serialize to JSON using jsonrs (mandated by project linter rules).
	jsonBytes, err := jsonrs.Marshal(data)
	if err != nil {
		return fmt.Errorf("serialize profile %d for cache: %w", segmentID, err)
	}

	// Store in Redis with TTL for automatic expiration.
	if err := c.client.Set(ctx, key, string(jsonBytes), ttl).Err(); err != nil {
		return fmt.Errorf("redis set profile %d: %w", segmentID, err)
	}

	c.logger.Debugn("Cached profile",
		logger.NewIntField("segmentID", segmentID),
		logger.NewStringField("ttl", ttl.String()),
	)

	return nil
}

// InvalidateProfile removes a cached profile from Redis. This must be called
// whenever the identity graph is updated for a segment (e.g., by ProcessEvent
// in identity/graph/graph.go) to prevent stale data from being served by the
// Profiles API.
//
// The operation is idempotent — deleting a non-existent key is not an error.
// Redis errors are logged at WARN level (non-critical but noteworthy) and
// returned to the caller for optional handling.
func (c *RedisProfileCache) InvalidateProfile(ctx context.Context, segmentID int64) error {
	key := cacheKey(segmentID)

	if err := c.client.Del(ctx, key).Err(); err != nil {
		c.logger.Warnn("Cache invalidation error",
			logger.NewIntField("segmentID", segmentID),
			obskit.Error(err),
		)
		return fmt.Errorf("redis del profile %d: %w", segmentID, err)
	}

	c.logger.Debugn("Invalidated cached profile",
		logger.NewIntField("segmentID", segmentID),
	)

	return nil
}

// NoopCache is a no-operation cache implementation that silently ignores all
// cache operations. It is used when Redis is not configured, enabling graceful
// degradation where the Profiles API still works but without caching — every
// request goes directly to the identity graph.
//
// NoopCache satisfies the ProfileCache interface.
type NoopCache struct{}

// GetProfile always returns (nil, nil) for NoopCache, simulating a perpetual
// cache miss. The caller will always fall through to the identity graph query.
func (n *NoopCache) GetProfile(_ context.Context, _ int64) (*storage.ProfileData, error) {
	return nil, nil //nolint:nilnil // nil,nil signals cache miss per ProfileCache contract
}

// SetProfile is a no-op for NoopCache. Cache writes are silently ignored.
func (n *NoopCache) SetProfile(_ context.Context, _ int64, _ *storage.ProfileData, _ time.Duration) error {
	return nil
}

// InvalidateProfile is a no-op for NoopCache. Invalidation requests are
// silently ignored since there is no cached data to remove.
func (n *NoopCache) InvalidateProfile(_ context.Context, _ int64) error {
	return nil
}

// Compile-time interface satisfaction checks. These assertions verify that
// RedisProfileCache and NoopCache both implement ProfileCache at compile time,
// catching interface drift early.
var (
	_ ProfileCache = (*RedisProfileCache)(nil)
	_ ProfileCache = (*NoopCache)(nil)
)
