package db

import (
    "context"
    "fmt"

    "github.com/redis/go-redis/v9"
    "github.com/rs/zerolog/log"
)

func NewRedis(ctx context.Context, addr, password string, dbIndex int) (*redis.Client, error) {
    client := redis.NewClient(&redis.Options{
        Addr:         addr,
        Password:     password,
        DB:           dbIndex,
        PoolSize:     20,
        MinIdleConns: 5,
    })

    if err := client.Ping(ctx).Err(); err != nil {
        return nil, fmt.Errorf("pinging Redis: %w", err)
    }

    log.Info().Str("addr", addr).Msg("Redis connection established")
    return client, nil
}