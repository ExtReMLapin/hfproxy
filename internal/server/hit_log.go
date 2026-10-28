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

func (s *Server) drainHitLog(h *hitLog) {
	for {
		batch := make([]db.DownloadEvent, 0, hitLogBatch)
		for len(batch) < hitLogBatch {
			select {
			case event := <-h.events:
				batch = append(batch, event)
			default:
				s.writeHitBatch(batch)
				return
			}
		}
		s.writeHitBatch(batch)
	}
}

func (s *Server) writeHitBatch(batch []db.DownloadEvent) {
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
