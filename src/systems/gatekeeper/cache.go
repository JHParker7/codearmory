package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

const entityTTL = 5 * time.Minute

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

func cacheGet[T any](ctx context.Context, key string) (T, bool) {
	var zero T
	if redisClient == nil {
		return zero, false
	}
	data, err := redisClient.Get(ctx, key).Bytes()
	if err != nil {
		return zero, false
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		slog.WarnContext(ctx, "cache: unmarshal failed", "key", key, "error", err)
		return zero, false
	}
	return v, true
}

func cacheSet(ctx context.Context, key string, val any, ttl time.Duration) {
	if redisClient == nil || ttl <= 0 {
		return
	}
	data, err := json.Marshal(val)
	if err != nil {
		slog.WarnContext(ctx, "cache: marshal failed", "key", key, "error", err)
		return
	}
	if err := redisClient.Set(ctx, key, data, ttl).Err(); err != nil {
		slog.WarnContext(ctx, "cache: set failed", "key", key, "error", err)
	}
}

func cacheDel(ctx context.Context, keys ...string) {
	if redisClient == nil || len(keys) == 0 {
		return
	}
	if err := redisClient.Del(ctx, keys...).Err(); err != nil {
		slog.WarnContext(ctx, "cache: del failed", "keys", keys, "error", err)
	}
}

// rlScript atomically increments a rate-limit counter and sets its TTL on
// first use, so the window expires naturally without a separate cleanup pass.
var rlScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then
    redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return count
`)

// redisRateLimit checks whether ip has exceeded maxAttempts within the current
// fixed window for endpoint. Returns true (allowed) when Redis is unavailable.
func redisRateLimit(ctx context.Context, endpoint, ip string, maxAttempts int, window time.Duration) bool {
	if redisClient == nil {
		return true
	}
	secs := int64(window.Seconds())
	if secs <= 0 {
		secs = 1 // sub-second windows truncate to 0; guard against divide-by-zero panic
	}
	bucket := time.Now().Unix() / secs
	key := fmt.Sprintf("gk:rl:%s:%s:%d", endpoint, ip, bucket)
	n, err := rlScript.Run(ctx, redisClient, []string{key}, secs*2).Int64()
	if err != nil {
		return true
	}
	return n <= int64(maxAttempts)
}

// cacheTrackUserSession records a session ID under the user's session set so
// that all sessions can be bulk-invalidated when the user is deleted.
func cacheTrackUserSession(ctx context.Context, userID, sessionID string) {
	if redisClient == nil {
		return
	}
	key := "gk:user:" + userID + ":sessions"
	redisClient.SAdd(ctx, key, sessionID)
	// Sessions live at most 24 h; give the set a bit of headroom.
	redisClient.Expire(ctx, key, 25*time.Hour)
}

// cacheDelUserSessions invalidates every cached session belonging to userID.
func cacheDelUserSessions(ctx context.Context, userID string) {
	if redisClient == nil {
		return
	}
	setKey := "gk:user:" + userID + ":sessions"
	sids, err := redisClient.SMembers(ctx, setKey).Result()
	if err != nil || len(sids) == 0 {
		cacheDel(ctx, setKey)
		return
	}
	keys := make([]string, 0, len(sids)+1)
	for _, sid := range sids {
		keys = append(keys, "gk:session:"+sid)
	}
	keys = append(keys, setKey)
	cacheDel(ctx, keys...)
}
