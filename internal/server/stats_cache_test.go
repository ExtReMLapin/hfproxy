package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hfproxy/hfproxy/internal/db"
)

func TestStatsCacheReusesFiguresPerViewer(t *testing.T) {
	var calls atomic.Int64
	compute := func(context.Context) (db.Stats, error) {
		n := calls.Add(1)
		return db.Stats{Hits: n}, nil
	}
	c := &statsCache{ttl: time.Hour}
	ctx := context.Background()

	first, _ := c.get(ctx, "admin", "a1", compute)
	second, _ := c.get(ctx, "admin", "a2", compute) // another administrator shares the entry
	if first.Hits != 1 || second.Hits != 1 || calls.Load() != 1 {
		t.Fatalf("administrators should share one computation: %v %v after %d calls", first, second, calls.Load())
	}
	reader, _ := c.get(ctx, "reader", "u1", compute)
	other, _ := c.get(ctx, "reader", "u2", compute)
	again, _ := c.get(ctx, "reader", "u1", compute)
	if reader.Hits != 2 || other.Hits != 3 || again.Hits != 2 || calls.Load() != 3 {
		t.Fatalf("each reader has their own figures: %v %v %v after %d calls", reader, other, again, calls.Load())
	}
}

func TestStatsCacheExpiresAndDoesNotKeepErrors(t *testing.T) {
	var calls atomic.Int64
	fail := true
	compute := func(context.Context) (db.Stats, error) {
		calls.Add(1)
		if fail {
			return db.Stats{}, errors.New("database down")
		}
		return db.Stats{Hits: calls.Load()}, nil
	}
	c := &statsCache{ttl: 20 * time.Millisecond}
	ctx := context.Background()
	if _, err := c.get(ctx, "admin", "a", compute); err == nil {
		t.Fatal("the error was swallowed")
	}
	fail = false
	got, err := c.get(ctx, "admin", "a", compute)
	if err != nil || got.Hits != 2 {
		t.Fatalf("after an error the next call must compute again: %v %v", got, err)
	}
	time.Sleep(40 * time.Millisecond)
	got, _ = c.get(ctx, "admin", "a", compute)
	if got.Hits != 3 {
		t.Fatalf("an expired entry must be recomputed, got %v", got)
	}
}

func TestStatsCacheComputesOnceForConcurrentRequests(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	compute := func(context.Context) (db.Stats, error) {
		calls.Add(1)
		<-release
		return db.Stats{Artifacts: 7}, nil
	}
	c := &statsCache{ttl: time.Hour}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := c.get(context.Background(), "admin", "a", compute); err != nil || got.Artifacts != 7 {
				t.Errorf("got %v %v", got, err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("%d concurrent requests ran the query %d times, want once", 20, calls.Load())
	}
}

func TestStatsCacheIsOffWithoutATTL(t *testing.T) {
	var calls atomic.Int64
	compute := func(context.Context) (db.Stats, error) { calls.Add(1); return db.Stats{}, nil }
	c := &statsCache{}
	for i := 0; i < 3; i++ {
		_, _ = c.get(context.Background(), "admin", "a", compute)
	}
	if calls.Load() != 3 {
		t.Fatalf("without a ttl every call computes: %d", calls.Load())
	}
}
