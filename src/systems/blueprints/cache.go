package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

const stateTTL = 5 * time.Minute

var redisClient *redis.Client

func initCache() {
	url := secret("REDIS_URL")
	if url == "" {
		slog.Info("redis cache disabled (REDIS_URL / REDIS_URL_FILE not set)")
		return
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		slog.Warn("invalid REDIS_URL / REDIS_URL_FILE, cache disabled", "error", err)
		return
	}
	c := redis.NewClient(opt)
	if err := c.Ping(context.Background()).Err(); err != nil {
		slog.Warn("redis unreachable, cache disabled", "error", err)
		return
	}
	redisClient = c
	slog.Info("redis cache connected")
}

func stateCacheGet(ctx context.Context, key string) ([]byte, bool) {
	if redisClient == nil {
		return nil, false
	}
	data, err := redisClient.Get(ctx, "bp:state:"+key).Bytes()
	if err != nil {
		return nil, false
	}
	return data, true
}

func stateCacheSet(ctx context.Context, key string, data []byte) {
	if redisClient == nil {
		return
	}
	if err := redisClient.Set(ctx, "bp:state:"+key, data, stateTTL).Err(); err != nil {
		slog.WarnContext(ctx, "cache: set failed", "key", key, "error", err)
	}
}

func stateCacheDel(ctx context.Context, key string) {
	if redisClient == nil {
		return
	}
	if err := redisClient.Del(ctx, "bp:state:"+key).Err(); err != nil {
		slog.WarnContext(ctx, "cache: del failed", "key", key, "error", err)
	}
}
