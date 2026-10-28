package server

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/hfproxy/hfproxy/internal/config"
	"github.com/hfproxy/hfproxy/internal/db"
	"github.com/hfproxy/hfproxy/internal/storage"
)

func TestCollectOrphanedBlobsPropagatesPostgreSQLFailure(t *testing.T) {
	sqlDB, err := sql.Open("pgx", "postgres://invalid")
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{db: &db.Store{SQL: sqlDB}}
	s.runtime.Store(&runtimeState{
		config:  config.Config{CacheGCGrace: time.Hour},
		storage: &storage.S3{},
	})
	if err := s.collectOrphanedBlobs(context.Background()); err == nil {
		t.Fatal("collectOrphanedBlobs unexpectedly succeeded after PostgreSQL became unavailable")
	}
}
