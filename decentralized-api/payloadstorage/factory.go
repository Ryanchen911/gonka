package payloadstorage

import (
	"context"
	"os"

	"decentralized-api/logging"

	"github.com/productscience/inference/x/inference/types"
)

// NewPayloadStorage creates a PayloadStorage based on environment configuration.
// If PGHOST is set and PostgreSQL is accessible, uses HybridStorage (PG primary + file fallback).
// Otherwise, uses FileStorage only.
func NewPayloadStorage(ctx context.Context, fileBasePath string) PayloadStorage {
	fileStorage := NewFileStorage(fileBasePath)

	pgHost := os.Getenv("PGHOST")
	if pgHost == "" {
		logging.Info("PGHOST not set, using file storage only", types.PayloadStorage)
		return fileStorage
	}

	pgStorage, err := NewPostgresStorage(ctx)
	if err != nil {
		logging.Error("PostgreSQL configured but connection failed, falling back to file storage", types.PayloadStorage,
			"host", pgHost, "error", err)
		return fileStorage
	}

	logging.Info("Using PostgreSQL with file fallback", types.PayloadStorage, "host", pgHost)
	return NewHybridStorage(pgStorage, fileStorage)
}
