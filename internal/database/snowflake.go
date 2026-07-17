package database

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// SnowflakeEpoch is the custom epoch (2024-01-01 00:00:00 UTC) used to
// reduce the bit width of the timestamp component.
const SnowflakeEpoch = 1704067200000 // 2024-01-01 in milliseconds

// ErrSnowflakeUnavailable indicates that an ID could not be generated before
// the rollback timeout or caller context expired.
var ErrSnowflakeUnavailable = errors.New("snowflake ID generator unavailable")

// SnowflakeGenerator produces 64-bit IDs with the layout:
//
//	1 bit sign (always 0) | 41 bit timestamp (ms since epoch) | 10 bit machine | 12 bit sequence
//
// The generator is safe for concurrent use.
type SnowflakeGenerator struct {
	mu        sync.Mutex
	machineID int64
	lastTS    int64
	seq       int64
	nowMillis func() int64
	timeout   time.Duration
}

// NewSnowflakeGenerator creates a generator with the given machine ID (0-1023).
func NewSnowflakeGenerator(machineID int64, rollbackTimeout ...time.Duration) *SnowflakeGenerator {
	timeout := 5 * time.Second
	if len(rollbackTimeout) > 0 {
		timeout = rollbackTimeout[0]
	}
	return newSnowflakeGenerator(machineID, timeout, func() int64 { return time.Now().UnixMilli() })
}

func newSnowflakeGenerator(machineID int64, timeout time.Duration, nowMillis func() int64) *SnowflakeGenerator {
	return &SnowflakeGenerator{
		machineID: machineID & 0x3FF,
		lastTS:    -1,
		nowMillis: nowMillis,
		timeout:   timeout,
	}
}

// NextID generates a new snowflake ID, waiting for clock recovery only up to
// the configured rollback timeout and respecting caller cancellation.
func (g *SnowflakeGenerator) NextID(ctx context.Context) (int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.nowMillis() - SnowflakeEpoch
	if now < g.lastTS {
		var err error
		now, err = g.waitAfter(ctx, g.lastTS-1)
		if err != nil {
			return 0, err
		}
	}
	if now == g.lastTS {
		g.seq = (g.seq + 1) & 0xFFF
		if g.seq == 0 {
			var err error
			now, err = g.waitAfter(ctx, g.lastTS)
			if err != nil {
				return 0, err
			}
		}
	} else {
		g.seq = 0
	}
	g.lastTS = now

	return (now << 22) | (g.machineID << 12) | g.seq, nil
}

func (g *SnowflakeGenerator) waitAfter(ctx context.Context, timestamp int64) (int64, error) {
	waitCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		now := g.nowMillis() - SnowflakeEpoch
		if now > timestamp {
			return now, nil
		}
		select {
		case <-waitCtx.Done():
			return 0, fmt.Errorf("%w: clock has not advanced past %d: %v", ErrSnowflakeUnavailable, timestamp, waitCtx.Err())
		case <-ticker.C:
		}
	}
}
