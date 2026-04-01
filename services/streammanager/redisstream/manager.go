//go:generate mockgen -destination=../../../mocks/services/streammanager/redisstream/mock_redisstream.go -package mock_redisstream github.com/rudderlabs/rudder-server/services/streammanager/redisstream RedisStreamClient

package redisstream

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"

	"github.com/redis/go-redis/v9"
	"github.com/tidwall/gjson"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	"github.com/rudderlabs/rudder-server/services/streammanager/common"
)

// pkgLogger is the package-scoped structured logger for the redisstream package,
// initialised in init() following the same pattern as firehose, kinesis, and other
// stream-manager producer packages.
var pkgLogger logger.Logger

func init() {
	pkgLogger = logger.NewLogger().Child("streammanager").Child("redisstream")
}

// Config holds the Redis Streams destination configuration parsed from the
// backend-config DestinationT.Config map. All fields are populated via JSON
// unmarshalling of the destination's config payload.
type Config struct {
	// Address is the Redis server address in "host:port" format.
	Address string `json:"address"`
	// Stream is the target Redis stream name for XADD operations.
	Stream string `json:"stream"`
	// ClusterMode enables Redis Cluster client when true; uses single-node client otherwise.
	ClusterMode bool `json:"clusterMode"`
	// UseTLS enables TLS with a minimum version of TLS 1.2 for the Redis connection.
	UseTLS bool `json:"useTLS"`
	// Password is the Redis AUTH password (optional).
	Password string `json:"password"`
	// Username is the Redis ACL username (optional, requires Redis 6+).
	Username string `json:"username"`
}

// RedisStreamClient abstracts Redis stream operations for testability.
// The interface wraps XAdd with a simple (string, error) return value instead
// of *redis.StringCmd so that gomock can return primitive pairs without
// constructing SDK-internal command objects.
type RedisStreamClient interface {
	// XAdd appends a new entry to the specified Redis stream and returns the
	// auto-generated entry ID on success.
	XAdd(ctx context.Context, stream string, values map[string]interface{}) (string, error)
	// Close releases all resources held by the underlying Redis client,
	// including connection pool handles.
	Close() error
}

// redisClient wraps a single-node *redis.Client and satisfies RedisStreamClient.
type redisClient struct {
	client *redis.Client
}

// XAdd delegates to the underlying redis.Client, constructing an XAddArgs from
// the provided stream name and field map, and calls .Result() to unpack the
// StringCmd into a plain (string, error) tuple.
func (c *redisClient) XAdd(ctx context.Context, stream string, values map[string]interface{}) (string, error) {
	return c.client.XAdd(ctx, &redis.XAddArgs{
		Stream: stream,
		Values: values,
	}).Result()
}

// Close shuts down the single-node Redis client and releases its connection pool.
func (c *redisClient) Close() error {
	return c.client.Close()
}

// redisClusterClient wraps *redis.ClusterClient and satisfies RedisStreamClient,
// enabling XADD operations against a Redis Cluster deployment.
type redisClusterClient struct {
	client *redis.ClusterClient
}

// XAdd delegates to the underlying redis.ClusterClient, constructing an XAddArgs
// from the provided stream name and field map, and calls .Result() to unpack the
// StringCmd into a plain (string, error) tuple.
func (c *redisClusterClient) XAdd(ctx context.Context, stream string, values map[string]interface{}) (string, error) {
	return c.client.XAdd(ctx, &redis.XAddArgs{
		Stream: stream,
		Values: values,
	}).Result()
}

// Close shuts down the cluster Redis client and releases all connection pool handles.
func (c *redisClusterClient) Close() error {
	return c.client.Close()
}

// RedisStreamProducer implements common.StreamProducer for the REDIS_STREAM
// destination type. It uses the RedisStreamClient abstraction so that tests
// can inject a mock without requiring a live Redis instance.
type RedisStreamProducer struct {
	client     RedisStreamClient
	streamName string
}

// NewProducer creates a new RedisStreamProducer based on the backend-config
// destination configuration. It validates required fields (address, stream),
// constructs either a single-node or cluster Redis client depending on the
// ClusterMode flag, and applies TLS and timeout settings as configured.
//
// All error messages include "Redis" so that the stream-manager factory tests
// can assert the correct producer was dispatched via:
//
//	assert.ErrorContains(t, err, "Redis")
func NewProducer(destination *backendconfig.DestinationT, opts common.Opts) (common.StreamProducer, error) {
	// Marshal the untyped config map to JSON so it can be unmarshalled into the
	// strongly-typed Config struct. This matches the firehose/kinesis pattern.
	configJSON, err := jsonrs.Marshal(destination.Config)
	if err != nil {
		return nil, fmt.Errorf("[RedisStream] error :: could not marshal Redis destination config: %w", err)
	}

	var config Config
	if err := jsonrs.Unmarshal(configJSON, &config); err != nil {
		return nil, fmt.Errorf("[RedisStream] error :: could not unmarshal Redis config: %w", err)
	}

	// Validate mandatory fields — address and stream name are both required
	// for any meaningful XADD operation.
	if config.Address == "" {
		return nil, fmt.Errorf("[RedisStream] error :: Redis address is required")
	}
	if config.Stream == "" {
		return nil, fmt.Errorf("[RedisStream] error :: Redis stream name is required")
	}

	var client RedisStreamClient

	if config.ClusterMode {
		// Cluster mode: use redis.ClusterClient with a single seed address.
		// In production the cluster client will discover additional nodes
		// automatically via CLUSTER SLOTS.
		clusterOpts := &redis.ClusterOptions{
			Addrs:    []string{config.Address},
			Password: config.Password,
			Username: config.Username,
		}
		if config.UseTLS {
			clusterOpts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		client = &redisClusterClient{client: redis.NewClusterClient(clusterOpts)}
	} else {
		// Single-node mode: use redis.Client with a direct address.
		redisOpts := &redis.Options{
			Addr:     config.Address,
			Password: config.Password,
			Username: config.Username,
		}
		if config.UseTLS {
			redisOpts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		// Propagate the stream-manager timeout to all Redis I/O operations
		// so that slow connections are bounded, analogous to the AWS SDK's
		// httpMaxIdleConnsPerHost configuration in the firehose producer.
		if opts.Timeout > 0 {
			redisOpts.DialTimeout = opts.Timeout
			redisOpts.ReadTimeout = opts.Timeout
			redisOpts.WriteTimeout = opts.Timeout
		}
		client = &redisClient{client: redis.NewClient(redisOpts)}
	}

	return &RedisStreamProducer{
		client:     client,
		streamName: config.Stream,
	}, nil
}

// Produce sends an event payload to the configured Redis stream via XADD.
//
// The jsonData parameter is the raw JSON event envelope produced by the Router.
// The "message" field is extracted using gjson, serialised to JSON, and written
// as a single "data" field in the Redis stream entry.
//
// Return values follow the (statusCode, status, responseMessage) convention
// used by all stream-manager producers:
//   - 200 / "Success" on successful XADD
//   - 400 / "Failure" for client-side errors (nil client, missing message, marshal failure)
//   - 500 / "Failure" for Redis server errors
func (producer *RedisStreamProducer) Produce(jsonData json.RawMessage, _ any) (int, string, string) {
	client := producer.client
	if client == nil {
		return 400, "Failure", "[RedisStream] error :: Could not create producer"
	}

	// Parse the full event envelope and extract the "message" sub-object.
	// This matches the firehose pattern: gjson.ParseBytes → .Get("message").
	parsedJSON := gjson.ParseBytes(jsonData)
	data := parsedJSON.Get("message").Value()
	if data == nil {
		return 400, "Failure", "[RedisStream] error :: message from payload not found"
	}

	// Serialise the message to JSON bytes for storage in the stream entry.
	value, err := jsonrs.Marshal(data)
	if err != nil {
		pkgLogger.Errorn("[RedisStream] error", obskit.Error(err))
		return 400, "Failure", "[RedisStream] error :: " + err.Error()
	}

	// Append the serialised message to the Redis stream using XADD.
	// The entry contains a single "data" field with the JSON string.
	messageID, err := client.XAdd(context.Background(), producer.streamName, map[string]interface{}{
		"data": string(value),
	})
	if err != nil {
		pkgLogger.Errorn("[RedisStream] error",
			logger.NewStringField("responseMessage", err.Error()))
		return 500, "Failure", "[RedisStream] error :: " + err.Error()
	}

	return 200, "Success", fmt.Sprintf("Message delivered with ID %s", messageID)
}

// Close disconnects the Redis client and releases all connection pool resources.
// It is safe to call on a producer with a nil client (returns nil).
func (producer *RedisStreamProducer) Close() error {
	if producer.client != nil {
		return producer.client.Close()
	}
	return nil
}
