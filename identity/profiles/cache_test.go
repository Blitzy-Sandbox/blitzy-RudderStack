package profiles

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"

	"github.com/rudderlabs/rudder-server/identity/storage"
)

// ---------------------------------------------------------------------------
// Mock Redis Client
// ---------------------------------------------------------------------------

// mockRedisClient implements the RedisClient interface using an in-memory map.
// It supports error injection via configurable error hooks and tracks calls
// for test assertions on key/TTL arguments.
type mockRedisClient struct {
	store    map[string]string
	getErr   error
	setErr   error
	delErr   error
	pingErr  error
	getCalls int
	setCalls int
	delCalls int

	lastGetKey  string
	lastSetKey  string
	lastSetTTL  time.Duration
	lastDelKeys []string
}

// Get returns a *redis.StringCmd with the stored value or redis.Nil on cache miss.
// Respects context cancellation and injected getErr.
func (m *mockRedisClient) Get(ctx context.Context, key string) *redis.StringCmd {
	m.getCalls++
	m.lastGetKey = key

	cmd := redis.NewStringCmd(ctx)

	// Respect context cancellation to support context handling tests.
	if ctx.Err() != nil {
		cmd.SetErr(ctx.Err())
		return cmd
	}

	if m.getErr != nil {
		cmd.SetErr(m.getErr)
		return cmd
	}

	if val, ok := m.store[key]; ok {
		cmd.SetVal(val)
	} else {
		cmd.SetErr(redis.Nil)
	}

	return cmd
}

// Set stores a value (expected to be a string) in the in-memory map.
// Tracks the key and TTL for test verification.
func (m *mockRedisClient) Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd {
	m.setCalls++
	m.lastSetKey = key
	m.lastSetTTL = expiration

	cmd := redis.NewStatusCmd(ctx)

	if ctx.Err() != nil {
		cmd.SetErr(ctx.Err())
		return cmd
	}

	if m.setErr != nil {
		cmd.SetErr(m.setErr)
		return cmd
	}

	// The cache always passes string values from jsonrs.Marshal output.
	m.store[key] = value.(string)
	cmd.SetVal("OK")
	return cmd
}

// Del removes keys from the in-memory map. Returns the count of deleted keys.
func (m *mockRedisClient) Del(ctx context.Context, keys ...string) *redis.IntCmd {
	m.delCalls++
	m.lastDelKeys = keys

	cmd := redis.NewIntCmd(ctx)

	if ctx.Err() != nil {
		cmd.SetErr(ctx.Err())
		return cmd
	}

	if m.delErr != nil {
		cmd.SetErr(m.delErr)
		return cmd
	}

	var deleted int64
	for _, key := range keys {
		if _, ok := m.store[key]; ok {
			delete(m.store, key)
			deleted++
		}
	}
	cmd.SetVal(deleted)
	return cmd
}

// Ping returns a *redis.StatusCmd with "PONG" or an injected error.
func (m *mockRedisClient) Ping(ctx context.Context) *redis.StatusCmd {
	cmd := redis.NewStatusCmd(ctx)
	if m.pingErr != nil {
		cmd.SetErr(m.pingErr)
		return cmd
	}
	cmd.SetVal("PONG")
	return cmd
}

// ---------------------------------------------------------------------------
// Test Helpers
// ---------------------------------------------------------------------------

// newTestCache creates a RedisProfileCache with a mock Redis client for unit testing.
// Returns the concrete *RedisProfileCache and the mock for call verification.
func newTestCache(t *testing.T) (*RedisProfileCache, *mockRedisClient) {
	t.Helper()
	mock := &mockRedisClient{
		store: make(map[string]string),
	}
	cache := &RedisProfileCache{
		client:     mock,
		defaultTTL: defaultCacheTTL,
		logger:     logger.NOP,
	}
	return cache, mock
}

// sampleProfileData returns a fully populated storage.ProfileData instance
// suitable for cache round-trip verification. All fields are populated
// including nullable MergedAt and MergedFrom on ExternalIDs.
func sampleProfileData() *storage.ProfileData {
	now := time.Now().UTC().Truncate(time.Microsecond)
	mergedAt := now.Add(-1 * time.Hour)
	var mergedFrom int64 = 42

	return &storage.ProfileData{
		Segment: storage.GraphSegment{
			ID:          1,
			WorkspaceID: "ws-test-001",
			SegmentID:   "seg-uuid-001",
			CreatedAt:   now,
		},
		ExternalIDs: []storage.ExternalID{
			{
				ID:              1,
				GraphID:         1,
				WorkspaceID:     "ws-test-001",
				ExternalIDType:  "user_id",
				ExternalIDValue: "user-123",
				CreatedSource:   "javascript",
				CreatedAt:       now,
				MergedAt:        &mergedAt,
				MergedFrom:      &mergedFrom,
			},
			{
				ID:              2,
				GraphID:         1,
				WorkspaceID:     "ws-test-001",
				ExternalIDType:  "email",
				ExternalIDValue: "user@example.com",
				CreatedSource:   "server",
				CreatedAt:       now,
				MergedAt:        nil,
				MergedFrom:      nil,
			},
		},
		Traits: []storage.Trait{
			{ID: 1, GraphID: 1, Key: "name", Value: "Alice", UpdatedAt: now},
			{ID: 2, GraphID: 1, Key: "plan", Value: "enterprise", UpdatedAt: now},
		},
	}
}

// ---------------------------------------------------------------------------
// Phase 2: Cache Key Generation Tests
// ---------------------------------------------------------------------------

// TestCacheKey_Format verifies the deterministic "profile:{segmentID}" key format.
func TestCacheKey_Format(t *testing.T) {
	// Standard segment ID produces expected format.
	require.Equal(t, "profile:123", cacheKey(123))

	// Deterministic: same input always yields same output.
	require.Equal(t, cacheKey(123), cacheKey(123))

	// Different segment IDs produce different cache keys.
	require.NotEqual(t, cacheKey(1), cacheKey(2))

	// Edge case: zero segment ID.
	require.Equal(t, "profile:0", cacheKey(0))

	// Edge case: maximum int64 value.
	require.Equal(t, "profile:9223372036854775807", cacheKey(math.MaxInt64))

	// Negative segment IDs (defensive — IDs should be positive, but key must still be valid).
	require.Equal(t, "profile:-1", cacheKey(-1))
}

// TestCacheKey_Consistency verifies cache key generation is fully deterministic.
func TestCacheKey_Consistency(t *testing.T) {
	const segmentID int64 = 54321

	keys := make([]string, 100)
	for i := range keys {
		keys[i] = cacheKey(segmentID)
	}

	// Every generated key must be identical.
	for i := 1; i < len(keys); i++ {
		require.Equal(t, keys[0], keys[i])
	}
}

// ---------------------------------------------------------------------------
// Phase 3: Cache Get/Set Tests
// ---------------------------------------------------------------------------

// TestCache_SetAndGet_Success verifies a full set-then-get round-trip.
func TestCache_SetAndGet_Success(t *testing.T) {
	cache, mock := newTestCache(t)
	ctx := context.Background()
	profileData := sampleProfileData()

	// Set profile in cache.
	err := cache.SetProfile(ctx, 1, profileData, 5*time.Minute)
	require.NoError(t, err)

	// Verify mock tracked the Set call with correct key and TTL.
	require.Equal(t, 1, mock.setCalls)
	require.Equal(t, "profile:1", mock.lastSetKey)
	require.Equal(t, 5*time.Minute, mock.lastSetTTL)

	// Get profile from cache.
	retrieved, err := cache.GetProfile(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, retrieved)

	// Verify top-level segment data.
	require.Equal(t, profileData.Segment.ID, retrieved.Segment.ID)
	require.Equal(t, profileData.Segment.WorkspaceID, retrieved.Segment.WorkspaceID)
	require.Equal(t, profileData.Segment.SegmentID, retrieved.Segment.SegmentID)

	// Verify external IDs count and content.
	require.Len(t, retrieved.ExternalIDs, 2)
	require.Equal(t, "user_id", retrieved.ExternalIDs[0].ExternalIDType)
	require.Equal(t, "user-123", retrieved.ExternalIDs[0].ExternalIDValue)
	require.Equal(t, "email", retrieved.ExternalIDs[1].ExternalIDType)
	require.Equal(t, "user@example.com", retrieved.ExternalIDs[1].ExternalIDValue)

	// Verify traits count and content.
	require.Len(t, retrieved.Traits, 2)
	require.Equal(t, "name", retrieved.Traits[0].Key)
	require.Equal(t, "Alice", retrieved.Traits[0].Value)
	require.Equal(t, "plan", retrieved.Traits[1].Key)
	require.Equal(t, "enterprise", retrieved.Traits[1].Value)
}

// TestCache_Get_CacheMiss verifies that a non-existent key returns (nil, nil).
func TestCache_Get_CacheMiss(t *testing.T) {
	cache, mock := newTestCache(t)
	ctx := context.Background()

	data, err := cache.GetProfile(ctx, 999)
	require.NoError(t, err)
	require.Nil(t, data)

	// Verify the correct cache key was looked up.
	require.Equal(t, 1, mock.getCalls)
	require.Equal(t, "profile:999", mock.lastGetKey)
}

// TestCache_Get_EmptyProfile verifies that a profile with empty slices
// (no external IDs, no traits) survives the round-trip and preserves the
// empty (non-nil) slice semantics.
func TestCache_Get_EmptyProfile(t *testing.T) {
	cache, _ := newTestCache(t)
	ctx := context.Background()

	emptyProfile := &storage.ProfileData{
		Segment: storage.GraphSegment{
			ID:          10,
			WorkspaceID: "ws-empty",
			SegmentID:   "seg-empty",
			CreatedAt:   time.Now().UTC().Truncate(time.Microsecond),
		},
		ExternalIDs: []storage.ExternalID{},
		Traits:      []storage.Trait{},
	}

	err := cache.SetProfile(ctx, 10, emptyProfile, 5*time.Minute)
	require.NoError(t, err)

	retrieved, err := cache.GetProfile(ctx, 10)
	require.NoError(t, err)
	require.NotNil(t, retrieved)
	require.Equal(t, emptyProfile.Segment.ID, retrieved.Segment.ID)
	require.Len(t, retrieved.ExternalIDs, 0)
	require.Len(t, retrieved.Traits, 0)
}

// ---------------------------------------------------------------------------
// Phase 4: Serialization Tests
// ---------------------------------------------------------------------------

// TestCache_Serialization_RoundTrip verifies that JSON serialization used by
// the cache (jsonrs) produces lossless round-trip results for storage.ProfileData.
func TestCache_Serialization_RoundTrip(t *testing.T) {
	profileData := sampleProfileData()

	// Step 1: Marshal with jsonrs (same library the cache uses).
	data, err := jsonrs.Marshal(profileData)
	require.NoError(t, err)
	require.NotNil(t, data)

	// Step 2: Unmarshal back.
	var decoded storage.ProfileData
	err = jsonrs.Unmarshal(data, &decoded)
	require.NoError(t, err)

	// Step 3: Verify all top-level segment fields.
	require.Equal(t, profileData.Segment.ID, decoded.Segment.ID)
	require.Equal(t, profileData.Segment.WorkspaceID, decoded.Segment.WorkspaceID)
	require.Equal(t, profileData.Segment.SegmentID, decoded.Segment.SegmentID)

	// Step 4: Verify ExternalID fields.
	require.Len(t, decoded.ExternalIDs, 2)
	require.Equal(t, profileData.ExternalIDs[0].ExternalIDType, decoded.ExternalIDs[0].ExternalIDType)
	require.Equal(t, profileData.ExternalIDs[0].ExternalIDValue, decoded.ExternalIDs[0].ExternalIDValue)
	require.Equal(t, profileData.ExternalIDs[0].GraphID, decoded.ExternalIDs[0].GraphID)

	// Step 5: Verify Trait fields.
	require.Len(t, decoded.Traits, 2)
	require.Equal(t, profileData.Traits[0].Key, decoded.Traits[0].Key)
	require.Equal(t, profileData.Traits[0].Value, decoded.Traits[0].Value)
	require.Equal(t, profileData.Traits[1].Key, decoded.Traits[1].Key)
	require.Equal(t, profileData.Traits[1].Value, decoded.Traits[1].Value)
}

// TestCache_Serialization_NullableFields verifies that nullable ExternalID fields
// (MergedAt *time.Time, MergedFrom *int64) survive JSON serialization correctly.
func TestCache_Serialization_NullableFields(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	mergedAt := now.Add(-2 * time.Hour)
	var mergedFrom int64 = 99

	// Profile with one ExternalID that has nullable fields set
	// and one that has them as nil.
	profileData := &storage.ProfileData{
		Segment: storage.GraphSegment{
			ID:          5,
			WorkspaceID: "ws-nullable",
			SegmentID:   "seg-null",
			CreatedAt:   now,
		},
		ExternalIDs: []storage.ExternalID{
			{
				ID:              10,
				GraphID:         5,
				WorkspaceID:     "ws-nullable",
				ExternalIDType:  "user_id",
				ExternalIDValue: "u-1",
				CreatedSource:   "js",
				CreatedAt:       now,
				MergedAt:        &mergedAt,
				MergedFrom:      &mergedFrom,
			},
			{
				ID:              11,
				GraphID:         5,
				WorkspaceID:     "ws-nullable",
				ExternalIDType:  "anonymous_id",
				ExternalIDValue: "a-1",
				CreatedSource:   "js",
				CreatedAt:       now,
				MergedAt:        nil,
				MergedFrom:      nil,
			},
		},
		Traits: []storage.Trait{},
	}

	data, err := jsonrs.Marshal(profileData)
	require.NoError(t, err)

	var decoded storage.ProfileData
	err = jsonrs.Unmarshal(data, &decoded)
	require.NoError(t, err)

	// First external ID: nullable fields should be populated.
	require.NotNil(t, decoded.ExternalIDs[0].MergedAt)
	require.NotNil(t, decoded.ExternalIDs[0].MergedFrom)
	require.Equal(t, *profileData.ExternalIDs[0].MergedFrom, *decoded.ExternalIDs[0].MergedFrom)

	// Second external ID: nullable fields should remain nil.
	require.Nil(t, decoded.ExternalIDs[1].MergedAt)
	require.Nil(t, decoded.ExternalIDs[1].MergedFrom)
}

// TestCache_Serialization_TimestampPreservation verifies that specific timestamps
// (CreatedAt, UpdatedAt, MergedAt) survive JSON serialization at microsecond precision.
func TestCache_Serialization_TimestampPreservation(t *testing.T) {
	// Use a specific fixed time truncated to microseconds (Go JSON default resolution).
	refTime := time.Date(2025, 6, 15, 10, 30, 45, 123456000, time.UTC)
	mergedTime := time.Date(2025, 6, 14, 8, 0, 0, 0, time.UTC)
	var mergedFrom int64 = 7

	profileData := &storage.ProfileData{
		Segment: storage.GraphSegment{
			ID:          3,
			WorkspaceID: "ws-ts",
			SegmentID:   "seg-ts",
			CreatedAt:   refTime,
		},
		ExternalIDs: []storage.ExternalID{
			{
				ID:              20,
				GraphID:         3,
				WorkspaceID:     "ws-ts",
				ExternalIDType:  "email",
				ExternalIDValue: "ts@example.com",
				CreatedSource:   "server",
				CreatedAt:       refTime,
				MergedAt:        &mergedTime,
				MergedFrom:      &mergedFrom,
			},
		},
		Traits: []storage.Trait{
			{ID: 30, GraphID: 3, Key: "age", Value: "30", UpdatedAt: refTime},
		},
	}

	// Set and get through the actual cache.
	cache, _ := newTestCache(t)
	ctx := context.Background()

	err := cache.SetProfile(ctx, 3, profileData, 5*time.Minute)
	require.NoError(t, err)

	retrieved, err := cache.GetProfile(ctx, 3)
	require.NoError(t, err)
	require.NotNil(t, retrieved)

	// Verify timestamps match at microsecond precision.
	require.Equal(t, refTime, retrieved.Segment.CreatedAt)
	require.Equal(t, refTime, retrieved.ExternalIDs[0].CreatedAt)
	require.Equal(t, mergedTime, *retrieved.ExternalIDs[0].MergedAt)
	require.Equal(t, refTime, retrieved.Traits[0].UpdatedAt)
}

// TestCache_Deserialization_InvalidJSON verifies that corrupted JSON in the cache
// is handled gracefully, returning an error rather than panicking.
func TestCache_Deserialization_InvalidJSON(t *testing.T) {
	cache, mock := newTestCache(t)
	ctx := context.Background()

	// Inject raw invalid JSON directly into the mock store.
	mock.store[cacheKey(42)] = "{{invalid-json!!"

	data, err := cache.GetProfile(ctx, 42)
	require.Error(t, err)
	require.Nil(t, data)
	require.Contains(t, err.Error(), "deserialize cached profile")

	// Verify the corrupted entry was cleaned up (deleted).
	_, exists := mock.store[cacheKey(42)]
	require.Equal(t, false, exists)
}

// ---------------------------------------------------------------------------
// Phase 5: TTL Configuration Tests
// ---------------------------------------------------------------------------

// TestCache_TTL_FromConfig verifies that the cache TTL is read from
// the "Identity.Profiles.Cache.TTL" configuration key.
func TestCache_TTL_FromConfig(t *testing.T) {
	mock := &mockRedisClient{store: make(map[string]string)}
	conf := config.New()
	conf.Set("Identity.Profiles.Cache.TTL", 120) // 120 seconds = 2 minutes

	cache := NewRedisProfileCache(mock, conf, logger.NOP)
	ctx := context.Background()

	profileData := sampleProfileData()
	err := cache.SetProfile(ctx, 1, profileData, 0) // TTL 0 means use default
	require.NoError(t, err)

	// The mock should have received the configured TTL (120 seconds).
	require.Equal(t, 120*time.Second, mock.lastSetTTL)
}

// TestCache_TTL_CustomOverride verifies that a caller-provided TTL overrides
// the configured default.
func TestCache_TTL_CustomOverride(t *testing.T) {
	cache, mock := newTestCache(t)
	ctx := context.Background()
	profileData := sampleProfileData()

	customTTL := 60 * time.Second
	err := cache.SetProfile(ctx, 1, profileData, customTTL)
	require.NoError(t, err)

	// The mock should have received the custom TTL, not the default.
	require.Equal(t, customTTL, mock.lastSetTTL)
}

// TestCache_TTL_Default verifies that when no explicit TTL config is provided,
// the default 5-minute TTL is used.
func TestCache_TTL_Default(t *testing.T) {
	mock := &mockRedisClient{store: make(map[string]string)}
	conf := config.New()
	// Do NOT set Identity.Profiles.Cache.TTL — rely on the default of 300 seconds.

	cache := NewRedisProfileCache(mock, conf, logger.NOP)
	ctx := context.Background()

	profileData := sampleProfileData()
	err := cache.SetProfile(ctx, 1, profileData, 0) // TTL 0 means use default
	require.NoError(t, err)

	// Default: 300 seconds = 5 minutes
	require.Equal(t, 300*time.Second, mock.lastSetTTL)
}

// ---------------------------------------------------------------------------
// Phase 6: Cache Invalidation Tests
// ---------------------------------------------------------------------------

// TestCache_Invalidate_Success verifies that invalidation removes a cached profile.
func TestCache_Invalidate_Success(t *testing.T) {
	cache, mock := newTestCache(t)
	ctx := context.Background()
	profileData := sampleProfileData()

	// Set a profile.
	err := cache.SetProfile(ctx, 1, profileData, 5*time.Minute)
	require.NoError(t, err)

	// Verify it exists.
	data, err := cache.GetProfile(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, data)

	// Invalidate.
	err = cache.InvalidateProfile(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, 1, mock.delCalls)

	// Verify cache now returns nil (miss).
	data, err = cache.GetProfile(ctx, 1)
	require.NoError(t, err)
	require.Nil(t, data)
}

// TestCache_Invalidate_NonExistent verifies that invalidating a non-existent
// key is a no-op (idempotent, no error).
func TestCache_Invalidate_NonExistent(t *testing.T) {
	cache, _ := newTestCache(t)
	ctx := context.Background()

	err := cache.InvalidateProfile(ctx, 999)
	require.NoError(t, err)
}

// TestCache_Invalidate_CalledOnUpdate verifies the invalidation contract:
// InvalidateProfile accepts a segmentID int64 parameter and removes the
// corresponding cache entry so that stale data is not served.
func TestCache_Invalidate_CalledOnUpdate(t *testing.T) {
	cache, mock := newTestCache(t)
	ctx := context.Background()
	profileData := sampleProfileData()

	// Set initial profile.
	err := cache.SetProfile(ctx, 100, profileData, 5*time.Minute)
	require.NoError(t, err)

	// Simulate update: invalidate the old cache entry.
	err = cache.InvalidateProfile(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, 1, mock.delCalls)
	require.Len(t, mock.lastDelKeys, 1)
	require.Equal(t, cacheKey(100), mock.lastDelKeys[0])

	// After invalidation, a Get should return nil (miss).
	data, err := cache.GetProfile(ctx, 100)
	require.NoError(t, err)
	require.Nil(t, data)
}

// ---------------------------------------------------------------------------
// Phase 7: Error Handling Tests
// ---------------------------------------------------------------------------

// TestCache_Get_RedisError verifies that Redis Get errors are propagated to callers.
func TestCache_Get_RedisError(t *testing.T) {
	cache, mock := newTestCache(t)
	ctx := context.Background()

	mock.getErr = errors.New("redis connection refused")

	data, err := cache.GetProfile(ctx, 1)
	require.Error(t, err)
	require.Nil(t, data)
	require.Contains(t, err.Error(), "redis get profile")
	require.Contains(t, err.Error(), "redis connection refused")
}

// TestCache_Set_RedisError verifies that Redis Set errors are propagated to callers.
func TestCache_Set_RedisError(t *testing.T) {
	cache, mock := newTestCache(t)
	ctx := context.Background()

	mock.setErr = errors.New("redis write timeout")

	profileData := sampleProfileData()
	err := cache.SetProfile(ctx, 1, profileData, 5*time.Minute)
	require.Error(t, err)
	require.Contains(t, err.Error(), "redis set profile")
	require.Contains(t, err.Error(), "redis write timeout")
}

// TestCache_Invalidate_RedisError verifies that Redis Del errors are propagated
// to callers rather than being silently swallowed.
func TestCache_Invalidate_RedisError(t *testing.T) {
	cache, mock := newTestCache(t)
	ctx := context.Background()

	mock.delErr = errors.New("redis cluster unavailable")

	err := cache.InvalidateProfile(ctx, 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "redis del profile")
	require.Contains(t, err.Error(), "redis cluster unavailable")
}

// ---------------------------------------------------------------------------
// Phase 8: Context Handling Tests
// ---------------------------------------------------------------------------

// TestCache_ContextCancellation verifies that cache operations respect
// context cancellation. The mock propagates ctx.Err() as the error.
func TestCache_ContextCancellation(t *testing.T) {
	cache, _ := newTestCache(t)
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel context before calling operations.
	cancel()

	// GetProfile should fail with context.Canceled.
	data, err := cache.GetProfile(ctx, 1)
	require.Error(t, err)
	require.Nil(t, data)
	require.Contains(t, err.Error(), "context canceled")

	// SetProfile should fail with context.Canceled.
	profileData := sampleProfileData()
	err = cache.SetProfile(ctx, 1, profileData, 5*time.Minute)
	require.Error(t, err)
	require.Contains(t, err.Error(), "context canceled")

	// InvalidateProfile should fail with context.Canceled.
	err = cache.InvalidateProfile(ctx, 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "context canceled")
}

// TestCache_ContextTimeout verifies that cache operations fail gracefully
// when the context deadline is exceeded.
func TestCache_ContextTimeout(t *testing.T) {
	cache, _ := newTestCache(t)

	// Create a context that is already timed out.
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	// Allow the goroutine scheduler to register the timeout.
	time.Sleep(time.Millisecond)

	// GetProfile should fail with deadline exceeded.
	data, err := cache.GetProfile(ctx, 1)
	require.Error(t, err)
	require.Nil(t, data)

	// SetProfile should fail with deadline exceeded.
	profileData := sampleProfileData()
	err = cache.SetProfile(ctx, 1, profileData, 5*time.Minute)
	require.Error(t, err)

	// InvalidateProfile should fail with deadline exceeded.
	err = cache.InvalidateProfile(ctx, 1)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Phase 9: Constructor Tests
// ---------------------------------------------------------------------------

// TestNewRedisProfileCache verifies constructor returns a functional cache.
func TestNewRedisProfileCache(t *testing.T) {
	mock := &mockRedisClient{store: make(map[string]string)}
	conf := config.New()

	cache := NewRedisProfileCache(mock, conf, logger.NOP)
	require.NotNil(t, cache)

	// Verify the returned cache can perform operations.
	ctx := context.Background()
	profileData := sampleProfileData()

	err := cache.SetProfile(ctx, 1, profileData, 5*time.Minute)
	require.NoError(t, err)

	data, err := cache.GetProfile(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, data)
	require.Equal(t, profileData.Segment.ID, data.Segment.ID)
}

// TestNewRedisProfileCache_NilClient verifies graceful degradation when
// the Redis client is nil. The constructor returns a NoopCache that silently
// ignores all operations.
func TestNewRedisProfileCache_NilClient(t *testing.T) {
	conf := config.New()
	cache := NewRedisProfileCache(nil, conf, logger.NOP)
	require.NotNil(t, cache)

	// Verify NoopCache behavior: GetProfile returns (nil, nil).
	ctx := context.Background()
	data, err := cache.GetProfile(ctx, 1)
	require.NoError(t, err)
	require.Nil(t, data)

	// SetProfile is a no-op.
	err = cache.SetProfile(ctx, 1, sampleProfileData(), 5*time.Minute)
	require.NoError(t, err)

	// InvalidateProfile is a no-op.
	err = cache.InvalidateProfile(ctx, 1)
	require.NoError(t, err)
}

// TestNewRedisProfileCache_DefaultTTL verifies that when no TTL config is
// provided, the default 5-minute TTL is applied.
func TestNewRedisProfileCache_DefaultTTL(t *testing.T) {
	mock := &mockRedisClient{store: make(map[string]string)}
	// Pass nil config to trigger the default TTL path.
	cache := NewRedisProfileCache(mock, nil, logger.NOP)
	require.NotNil(t, cache)

	ctx := context.Background()
	profileData := sampleProfileData()

	// TTL 0 means use the configured default. With nil config, that's defaultCacheTTL.
	err := cache.SetProfile(ctx, 1, profileData, 0)
	require.NoError(t, err)

	// defaultCacheTTL = 5 * time.Minute
	require.Equal(t, 5*time.Minute, mock.lastSetTTL)
}

// ---------------------------------------------------------------------------
// Phase 10: ProfileCache Interface Tests
// ---------------------------------------------------------------------------

// TestProfileCache_InterfaceCompliance verifies that both RedisProfileCache and
// NoopCache correctly implement the ProfileCache interface, and that all three
// required methods (GetProfile, SetProfile, InvalidateProfile) are accessible
// through the interface.
func TestProfileCache_InterfaceCompliance(t *testing.T) {
	ctx := context.Background()
	profileData := sampleProfileData()

	// Verify RedisProfileCache implements ProfileCache.
	cache, _ := newTestCache(t)
	var redisIface ProfileCache = cache
	require.NotNil(t, redisIface)

	// Call all interface methods via the interface variable.
	data, err := redisIface.GetProfile(ctx, 1)
	require.NoError(t, err)
	require.Nil(t, data) // cache miss

	err = redisIface.SetProfile(ctx, 1, profileData, 5*time.Minute)
	require.NoError(t, err)

	data, err = redisIface.GetProfile(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, data)

	err = redisIface.InvalidateProfile(ctx, 1)
	require.NoError(t, err)

	// Verify NoopCache implements ProfileCache.
	var noopIface ProfileCache = &NoopCache{}
	require.NotNil(t, noopIface)

	data, err = noopIface.GetProfile(ctx, 1)
	require.NoError(t, err)
	require.Nil(t, data)

	err = noopIface.SetProfile(ctx, 1, profileData, 5*time.Minute)
	require.NoError(t, err)

	err = noopIface.InvalidateProfile(ctx, 1)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Additional Edge Case Tests
// ---------------------------------------------------------------------------

// TestCache_SetProfile_NilData verifies that SetProfile with nil data is a no-op.
func TestCache_SetProfile_NilData(t *testing.T) {
	cache, mock := newTestCache(t)
	ctx := context.Background()

	err := cache.SetProfile(ctx, 1, nil, 5*time.Minute)
	require.NoError(t, err)

	// No Redis call should have been made for nil data.
	require.Equal(t, 0, mock.setCalls)
}

// TestCache_MultipleSegments verifies that different segment IDs are cached
// independently in the same cache instance.
func TestCache_MultipleSegments(t *testing.T) {
	cache, _ := newTestCache(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Microsecond)

	profile1 := &storage.ProfileData{
		Segment: storage.GraphSegment{ID: 1, WorkspaceID: "ws-1", SegmentID: "seg-1", CreatedAt: now},
		Traits:  []storage.Trait{{ID: 1, GraphID: 1, Key: "name", Value: "Alice", UpdatedAt: now}},
	}
	profile2 := &storage.ProfileData{
		Segment: storage.GraphSegment{ID: 2, WorkspaceID: "ws-1", SegmentID: "seg-2", CreatedAt: now},
		Traits:  []storage.Trait{{ID: 2, GraphID: 2, Key: "name", Value: "Bob", UpdatedAt: now}},
	}

	// Set both profiles.
	require.NoError(t, cache.SetProfile(ctx, 1, profile1, 5*time.Minute))
	require.NoError(t, cache.SetProfile(ctx, 2, profile2, 5*time.Minute))

	// Retrieve and verify independence.
	data1, err := cache.GetProfile(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, data1)
	require.Equal(t, "Alice", data1.Traits[0].Value)

	data2, err := cache.GetProfile(ctx, 2)
	require.NoError(t, err)
	require.NotNil(t, data2)
	require.Equal(t, "Bob", data2.Traits[0].Value)

	// Invalidate one does not affect the other.
	require.NoError(t, cache.InvalidateProfile(ctx, 1))

	data1, err = cache.GetProfile(ctx, 1)
	require.NoError(t, err)
	require.Nil(t, data1) // invalidated

	data2, err = cache.GetProfile(ctx, 2)
	require.NoError(t, err)
	require.NotNil(t, data2) // still cached
	require.Equal(t, "Bob", data2.Traits[0].Value)
}

// TestCache_OverwriteExisting verifies that setting a profile for an existing
// segment ID overwrites the previous cached data.
func TestCache_OverwriteExisting(t *testing.T) {
	cache, _ := newTestCache(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	original := &storage.ProfileData{
		Segment: storage.GraphSegment{ID: 50, WorkspaceID: "ws-ow", SegmentID: "seg-ow", CreatedAt: now},
		Traits:  []storage.Trait{{ID: 1, GraphID: 50, Key: "plan", Value: "free", UpdatedAt: now}},
	}
	updated := &storage.ProfileData{
		Segment: storage.GraphSegment{ID: 50, WorkspaceID: "ws-ow", SegmentID: "seg-ow", CreatedAt: now},
		Traits:  []storage.Trait{{ID: 1, GraphID: 50, Key: "plan", Value: "enterprise", UpdatedAt: now}},
	}

	// Set original, then overwrite.
	require.NoError(t, cache.SetProfile(ctx, 50, original, 5*time.Minute))
	require.NoError(t, cache.SetProfile(ctx, 50, updated, 5*time.Minute))

	// Retrieve: should reflect the updated data.
	data, err := cache.GetProfile(ctx, 50)
	require.NoError(t, err)
	require.NotNil(t, data)
	require.Equal(t, "enterprise", data.Traits[0].Value)
}
