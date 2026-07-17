package database

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSnowflakeHandlesClockRollback(t *testing.T) {
	times := []int64{SnowflakeEpoch + 100, SnowflakeEpoch + 99, SnowflakeEpoch + 99, SnowflakeEpoch + 101, SnowflakeEpoch + 102}
	index := 0
	generator := newSnowflakeGenerator(7, time.Second, func() int64 {
		now := times[index]
		index++
		return now
	})

	const count = 3
	ids := make(map[int64]struct{}, count)
	var previous int64
	for range count {
		id, err := generator.NextID(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := ids[id]; exists {
			t.Fatalf("NextID() generated duplicate %d", id)
		}
		if id <= previous {
			t.Fatalf("NextID() = %d after %d, want increasing IDs", id, previous)
		}
		ids[id] = struct{}{}
		previous = id
	}
}

func TestSnowflakeRollbackTimesOut(t *testing.T) {
	generator := newSnowflakeGenerator(1, time.Millisecond, func() int64 { return SnowflakeEpoch - 1_000_000 })
	generator.lastTS = 200
	if _, err := generator.NextID(context.Background()); !errors.Is(err, ErrSnowflakeUnavailable) {
		t.Fatalf("NextID() error = %v, want ErrSnowflakeUnavailable", err)
	}
}

func TestSnowflakeConcurrentIDsAreUnique(t *testing.T) {
	generator := NewSnowflakeGenerator(42)
	const count = 10000
	ids := make(chan int64, count)
	var wg sync.WaitGroup

	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := generator.NextID(context.Background())
			if err != nil {
				t.Errorf("NextID() error = %v", err)
				return
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[int64]struct{}, count)
	for id := range ids {
		if _, exists := seen[id]; exists {
			t.Fatalf("NextID() generated duplicate %d", id)
		}
		seen[id] = struct{}{}
	}
}
