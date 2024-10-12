// redis_repository.go

package datarepository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	DefaultKeyPrefix     = "app"
	DefaultKeySeparator  = ":"
	DefaultKeyPartsCount = 3 // prefix:entityPrefix:id
	MinKeyLength         = 5
	MaxKeyLength         = 256
	KeyPartLock          = "lock"
	KeyPartPubSubChannel = "channel"
)

var (
	ErrEmptyKeyPart          = errors.New("empty key part used but not allowed")
	ErrInvalidKeyFormat      = errors.New("invalid key format")
	ErrInvalidKeyLength      = errors.New("key length out of allowed range")
	ErrInvalidKeyPrefix      = errors.New("key does not start with the correct prefix")
	ErrInvalidKeySuffix      = errors.New("key does not have at least one part after prefix")
	ErrInvalidKeyChars       = errors.New("key contains invalid characters")
	ErrInvalidEntityPrefix   = errors.New("invalid entity prefix: must start with a letter and contain only letters, numbers, and underscores")
	ErrUnsupportedIdentifier = errors.New("unsupported identifier type")

	validKeyRegex     = regexp.MustCompile(`^[a-zA-Z0-9_:.-]+$`)
	entityPrefixRegex = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)
)

type RedisConfig struct {
	Addrs            []string
	MasterName       string
	SentinelUsername string
	SentinelPassword string
	Username         string
	Password         string
	DB               int
	Mode             string
	KeyPrefix        string
	KeySeparator     string
}

func (c RedisConfig) GetConnectionString() string {
	return fmt.Sprintf("%s;%s;%s;%s;%s;%s;%d;%s",
		c.Mode, c.MasterName, c.SentinelUsername, c.SentinelPassword,
		c.Username, c.Password, c.DB, strings.Join(c.Addrs, ","))
}

type RedisIdentifier struct {
	EntityPrefix string
	ID           string
}

func (ri RedisIdentifier) String() string {
	return ri.EntityPrefix + ":" + ri.ID
}

type RedisRepository struct {
	client    redis.UniversalClient
	prefix    string
	separator string
}

func NewRedisRepository(config Config) (DataRepository, error) {
	redisConfig, ok := config.(RedisConfig)
	if !ok {
		return nil, fmt.Errorf("invalid config type for Redis repository")
	}

	options := &redis.UniversalOptions{
		Addrs:            redisConfig.Addrs,
		MasterName:       redisConfig.MasterName,
		SentinelUsername: redisConfig.SentinelUsername,
		SentinelPassword: redisConfig.SentinelPassword,
		Username:         redisConfig.Username,
		Password:         redisConfig.Password,
		DB:               redisConfig.DB,
	}

	client := redis.NewUniversalClient(options)

	prefix := redisConfig.KeyPrefix
	if prefix == "" {
		prefix = DefaultKeyPrefix
	}
	separator := redisConfig.KeySeparator
	if separator == "" {
		separator = DefaultKeySeparator
	}

	return &RedisRepository{
		client:    client,
		prefix:    prefix,
		separator: separator,
	}, nil
}

func (r *RedisRepository) validateKey(key string) error {
	if len(key) < MinKeyLength || len(key) > MaxKeyLength {
		return fmt.Errorf("%w: key length must be between %d and %d characters", ErrInvalidKeyLength, MinKeyLength, MaxKeyLength)
	}

	if !validKeyRegex.MatchString(key) {
		return fmt.Errorf("%w: key must contain only alphanumeric characters, underscores, colons, dots, and hyphens", ErrInvalidKeyChars)
	}

	if !strings.HasPrefix(key, r.prefix+r.separator) {
		return fmt.Errorf("%w: key must start with %s%s", ErrInvalidKeyPrefix, r.prefix, r.separator)
	}

	parts := strings.Split(key, r.separator)
	if len(parts) < 2 || parts[1] == "" {
		return fmt.Errorf("%w: key must have at least one non-empty part after the prefix", ErrInvalidKeySuffix)
	}

	for _, part := range parts {
		if part == "" {
			return ErrEmptyKeyPart
		}
	}

	return nil
}

func (r *RedisRepository) validateEntityPrefix(entityPrefix string) error {
	if !entityPrefixRegex.MatchString(entityPrefix) {
		return ErrInvalidEntityPrefix
	}
	return nil
}

func (r *RedisRepository) createKey(parts ...string) (string, error) {
	allParts := append([]string{r.prefix}, parts...)
	key := strings.Join(allParts, r.separator)
	if err := r.validateKey(key); err != nil {
		return "", err
	}
	return key, nil
}

func (r *RedisRepository) parseKey(key string) ([]string, error) {
	if err := r.validateKey(key); err != nil {
		return nil, err
	}
	return strings.Split(key, r.separator)[1:], nil
}

func (r *RedisRepository) identifierToKey(identifier EntityIdentifier) (string, error) {
	switch id := identifier.(type) {
	case RedisIdentifier:
		if err := r.validateEntityPrefix(id.EntityPrefix); err != nil {
			return "", err
		}
		return r.createKey(id.EntityPrefix, id.ID)
	case SimpleIdentifier:
		return r.createKey(string(id))
	default:
		return "", ErrUnsupportedIdentifier
	}
}

func (r *RedisRepository) keyToIdentifier(key string) (EntityIdentifier, error) {
	parts, err := r.parseKey(key)
	if err != nil {
		return nil, err
	}
	if len(parts) >= 2 {
		return RedisIdentifier{EntityPrefix: parts[0], ID: parts[1]}, nil
	}
	return SimpleIdentifier(strings.Join(parts, r.separator)), nil
}

func (r *RedisRepository) Create(ctx context.Context, identifier EntityIdentifier, value interface{}) error {
	key, err := r.identifierToKey(identifier)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidIdentifier, err)
	}

	exists, err := r.client.Exists(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOperationFailed, err)
	}
	if exists == 1 {
		return ErrAlreadyExists
	}

	return r.client.JSONSet(ctx, key, "$", value).Err()
}

func (r *RedisRepository) Read(ctx context.Context, identifier EntityIdentifier, value interface{}) error {
	key, err := r.identifierToKey(identifier)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidIdentifier, err)
	}

	data, err := r.client.JSONGet(ctx, key, "$").Result()
	if err != nil {
		if err == redis.Nil {
			return ErrNotFound
		}
		return fmt.Errorf("%w: %v", ErrOperationFailed, err)
	}

	return json.Unmarshal([]byte(data), value)
}

func (r *RedisRepository) Update(ctx context.Context, identifier EntityIdentifier, value interface{}) error {
	key, err := r.identifierToKey(identifier)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidIdentifier, err)
	}

	exists, err := r.client.Exists(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOperationFailed, err)
	}
	if exists == 0 {
		return ErrNotFound
	}

	return r.client.JSONSet(ctx, key, "$", value).Err()
}

func (r *RedisRepository) Delete(ctx context.Context, identifier EntityIdentifier) error {
	key, err := r.identifierToKey(identifier)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidIdentifier, err)
	}

	result, err := r.client.Del(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOperationFailed, err)
	}
	if result == 0 {
		return ErrNotFound
	}

	return nil
}

func (r *RedisRepository) List(ctx context.Context, pattern EntityIdentifier) ([]EntityIdentifier, error) {
	patternKey, err := r.identifierToKey(pattern)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidIdentifier, err)
	}

	keys, err := r.client.Keys(ctx, patternKey+"*").Result()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOperationFailed, err)
	}

	identifiers := make([]EntityIdentifier, 0, len(keys))
	for _, key := range keys {
		if err := r.validateKey(key); err != nil {
			continue // Skip invalid keys
		}
		identifier, err := r.keyToIdentifier(key)
		if err != nil {
			continue // Skip keys that can't be converted to identifiers
		}
		identifiers = append(identifiers, identifier)
	}

	return identifiers, nil
}

func (r *RedisRepository) Search(ctx context.Context, query string, offset, limit int, sortBy, sortDir string) ([]EntityIdentifier, error) {
	args := []interface{}{
		"FT.SEARCH", r.prefix, query,
		"LIMIT", offset, limit,
		"SORTBY", sortBy, sortDir,
	}
	res, err := r.client.Do(ctx, args...).Result()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOperationFailed, err)
	}

	array, ok := res.([]interface{})
	if !ok || len(array) < 1 {
		return nil, fmt.Errorf("unexpected search result format")
	}

	totalResults, ok := array[0].(int64)
	if !ok {
		return nil, fmt.Errorf("unexpected total results format")
	}

	if totalResults == 0 {
		return []EntityIdentifier{}, nil
	}

	identifiers := make([]EntityIdentifier, 0, totalResults)
	for i := 1; i < len(array); i += 2 {
		key, ok := array[i].(string)
		if !ok {
			continue // Skip invalid keys
		}
		if err := r.validateKey(key); err != nil {
			continue // Skip invalid keys
		}
		identifier, err := r.keyToIdentifier(key)
		if err != nil {
			continue // Skip keys that can't be converted to identifiers
		}
		identifiers = append(identifiers, identifier)
	}

	return identifiers, nil
}

func (r *RedisRepository) AcquireLock(ctx context.Context, identifier EntityIdentifier, ttl time.Duration) (bool, error) {
	key, err := r.identifierToKey(identifier)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrInvalidIdentifier, err)
	}
	lockKey := key + r.separator + KeyPartLock
	acquired, err := r.client.SetNX(ctx, lockKey, 1, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrOperationFailed, err)
	}
	return acquired, nil
}

func (r *RedisRepository) ReleaseLock(ctx context.Context, identifier EntityIdentifier) error {
	key, err := r.identifierToKey(identifier)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidIdentifier, err)
	}
	lockKey := key + r.separator + KeyPartLock
	result, err := r.client.Del(ctx, lockKey).Result()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOperationFailed, err)
	}
	if result == 0 {
		return ErrNotFound
	}

	return nil
}

func (r *RedisRepository) Publish(ctx context.Context, channel string, message interface{}) error {
	fullChannel := r.prefix + r.separator + KeyPartPubSubChannel + r.separator + channel
	return r.client.Publish(ctx, fullChannel, message).Err()
}

func (r *RedisRepository) Subscribe(ctx context.Context, channel string) (chan interface{}, error) {
	fullChannel := r.prefix + r.separator + KeyPartPubSubChannel + r.separator + channel
	pubsub := r.client.Subscribe(ctx, fullChannel)
	ch := make(chan interface{})

	go func() {
		defer close(ch)
		for msg := range pubsub.Channel() {
			ch <- msg.Payload
		}
	}()

	return ch, nil
}

func (r *RedisRepository) Ping(ctx context.Context) error {
	return r.client.Ping(ctx).Err()
}

func (r *RedisRepository) Close() error {
	return r.client.Close()
}
