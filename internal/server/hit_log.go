package server

import (
	"context"
	"time"

	"github.com/hfproxy/hfproxy/internal/db"
)

// hitLog writes the download events of cache hits in batches, off the request
// path. When the queue is full the caller writes its event itself, so nothing
// is dropped and a slow database slows requests down instead of losing the
// record of who downloaded what.
type hitLog struct {
	events chan db.DownloadEvent
	done   chan struct{}
}

const (
	hitLogBatch = 100
	hitLogDelay = 200 * time.Millisecond
)

// startHitLog turns on asynchronous logging of cache hits until ctx ends; the
// remaining events are written before the worker stops.
func (s *Server) startHitLog(ctx context.Context) {
	size := s.currentConfig().DownloadLogQueue
	if size <= 0 || s.hitLog != nil {
		return
	}
	h := &hitLog{events: make(chan db.DownloadEvent, size), done: make(chan struct{})}
	s.hitLog = h
	go func() {
		defer close(h.done)
		for {
			batch := make([]db.DownloadEvent, 0, hitLogBatch)
			select {
			case event := <-h.events:
				batch = append(batch, event)
			case <-ctx.Done():
				s.drainHitLog(h)
				return
			}
			timer := time.NewTimer(hitLogDelay)
		fill:
			for len(batch) < hitLogBatch {
				select {
				case event := <-h.events:
					batch = append(batch, event)
				case <-timer.C:
					break fill
				case <-ctx.Done():
					timer.Stop()
					s.writeHitBatch(batch)
					s.drainHitLog(h)
					return
				}
			}
			timer.Stop()
			s.writeHitBatch(batch)
		}
	}()
}

// hitLogDrainBudget bounds how long a stop spends writing the events still
// queued. With the database down every batch waits out its own timeout, which
// for a full queue would add minutes to the stop; what could not be written in
// time is dropped, and said so.
var hitLogDrainBudget = 15 * time.Second

func (s *Server) drainHitLog(h *hitLog) {
	deadline := time.Now().Add(hitLogDrainBudget)
	for {
		batch := make([]db.DownloadEvent, 0, hitLogBatch)
		for len(batch) < hitLogBatch {
			select {
			case event := <-h.events:
				batch = append(batch, event)
			default:
				s.writeHitBatchWithin(batch, time.Until(deadline))
				return
			}
		}
		if time.Until(deadline) <= 0 {
			s.logger.Warn("download events were dropped at shutdown: the database did not take them in time", "events", len(batch)+len(h.events))
			return
		}
		s.writeHitBatchWithin(batch, time.Until(deadline))
	}
}

func (s *Server) writeHitBatch(batch []db.DownloadEvent) {
	s.writeHitBatchWithin(batch, 10*time.Second)
}

func (s *Server) writeHitBatchWithin(batch []db.DownloadEvent, limit time.Duration) {
	if len(batch) == 0 {
		return
	}
	if limit > 10*time.Second {
		limit = 10 * time.Second
	}
	if limit <= 0 {
		s.logger.Warn("download events were dropped at shutdown: the database did not take them in time", "events", len(batch))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	if err := s.db.RecordDownloads(ctx, batch); err != nil {
		s.logger.Warn("download events could not be recorded", "events", len(batch))
	}
}

// enqueueHit queues an event, or reports false when logging is synchronous or
// the queue is full.
func (s *Server) enqueueHit(event db.DownloadEvent) bool {
	h := s.hitLog
	if h == nil {
		return false
	}
	select {
	case h.events <- event:
		return true
	default:
		return false
	}
}

// waitHitLog blocks until the worker has written everything after ctx ended.
func (s *Server) waitHitLog() {
	if h := s.hitLog; h != nil {
		<-h.done
	}
}
