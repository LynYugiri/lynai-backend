package database

import (
	"sync"
	"time"
)

// SnowflakeEpoch is the custom epoch (2024-01-01 00:00:00 UTC) used to
// reduce the bit width of the timestamp component.
const SnowflakeEpoch = 1704067200000 // 2024-01-01 in milliseconds

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
}

// NewSnowflakeGenerator creates a generator with the given machine ID (0-1023).
func NewSnowflakeGenerator(machineID int64) *SnowflakeGenerator {
	return &SnowflakeGenerator{machineID: machineID & 0x3FF}
}

// NextID generates a new snowflake ID.
func (g *SnowflakeGenerator) NextID() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := time.Now().UnixMilli() - SnowflakeEpoch
	if now == g.lastTS {
		g.seq = (g.seq + 1) & 0xFFF
		if g.seq == 0 {
			// Sequence exhausted for this millisecond — wait for next.
			for now <= g.lastTS {
				now = time.Now().UnixMilli() - SnowflakeEpoch
			}
		}
	} else {
		g.seq = 0
	}
	g.lastTS = now

	return (now << 22) | (g.machineID << 12) | g.seq
}
