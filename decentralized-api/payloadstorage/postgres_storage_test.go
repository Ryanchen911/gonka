package payloadstorage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func setupPostgresContainer(t *testing.T) (func(), error) {
	ctx := context.Background()

	container, err := postgres.Run(ctx,
		"postgres:18.1-bookworm",
		postgres.WithDatabase("testdb"),
		postgres.WithUsername("testuser"),
		postgres.WithPassword("testpass"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return nil, err
	}

	host, err := container.Host(ctx)
	if err != nil {
		container.Terminate(ctx)
		return nil, err
	}

	port, err := container.MappedPort(ctx, "5432")
	if err != nil {
		container.Terminate(ctx)
		return nil, err
	}

	// Set environment variables for pgx
	os.Setenv("PGHOST", host)
	os.Setenv("PGPORT", port.Port())
	os.Setenv("PGDATABASE", "testdb")
	os.Setenv("PGUSER", "testuser")
	os.Setenv("PGPASSWORD", "testpass")

	cleanup := func() {
		os.Unsetenv("PGHOST")
		os.Unsetenv("PGPORT")
		os.Unsetenv("PGDATABASE")
		os.Unsetenv("PGUSER")
		os.Unsetenv("PGPASSWORD")
		container.Terminate(ctx)
	}

	return cleanup, nil
}

func TestPostgresStorage_StoreAndRetrieve(t *testing.T) {
	cleanup, err := setupPostgresContainer(t)
	require.NoError(t, err)
	defer cleanup()

	ctx := context.Background()
	storage, err := NewPostgresStorage(ctx)
	require.NoError(t, err)
	defer storage.Close()

	// Test store
	err = storage.Store(ctx, "inf-001", 100, `{"prompt": "hello"}`, `{"response": "world"}`)
	require.NoError(t, err)

	// Test retrieve
	prompt, response, err := storage.Retrieve(ctx, "inf-001", 100)
	require.NoError(t, err)
	assert.Equal(t, `{"prompt": "hello"}`, prompt)
	assert.Equal(t, `{"response": "world"}`, response)

	// Test not found
	_, _, err = storage.Retrieve(ctx, "nonexistent", 100)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestPostgresStorage_PartitionAutoCreation(t *testing.T) {
	cleanup, err := setupPostgresContainer(t)
	require.NoError(t, err)
	defer cleanup()

	ctx := context.Background()
	storage, err := NewPostgresStorage(ctx)
	require.NoError(t, err)
	defer storage.Close()

	// Store in multiple epochs - partitions should be created automatically
	epochs := []uint64{100, 101, 102}
	for _, epoch := range epochs {
		err := storage.Store(ctx, "inf-001", epoch, `{"epoch": "`+string(rune(epoch))+`"}`, `{"resp": "ok"}`)
		require.NoError(t, err, "Failed to store in epoch %d", epoch)
	}

	// Verify all can be retrieved
	for _, epoch := range epochs {
		_, _, err := storage.Retrieve(ctx, "inf-001", epoch)
		require.NoError(t, err, "Failed to retrieve from epoch %d", epoch)
	}
}

func TestPostgresStorage_PruneEpoch(t *testing.T) {
	cleanup, err := setupPostgresContainer(t)
	require.NoError(t, err)
	defer cleanup()

	ctx := context.Background()
	storage, err := NewPostgresStorage(ctx)
	require.NoError(t, err)
	defer storage.Close()

	// Store data in epoch 100
	err = storage.Store(ctx, "inf-001", 100, `{"prompt": "test"}`, `{"response": "test"}`)
	require.NoError(t, err)

	// Verify it exists
	_, _, err = storage.Retrieve(ctx, "inf-001", 100)
	require.NoError(t, err)

	// Prune epoch 100
	err = storage.PruneEpoch(ctx, 100)
	require.NoError(t, err)

	// Verify it's gone
	_, _, err = storage.Retrieve(ctx, "inf-001", 100)
	assert.ErrorIs(t, err, ErrNotFound)

	// Prune non-existent epoch should not error
	err = storage.PruneEpoch(ctx, 999)
	require.NoError(t, err)
}

func TestPostgresStorage_SchemaAutoCreation(t *testing.T) {
	cleanup, err := setupPostgresContainer(t)
	require.NoError(t, err)
	defer cleanup()

	ctx := context.Background()

	// First connection creates schema
	storage1, err := NewPostgresStorage(ctx)
	require.NoError(t, err)
	storage1.Close()

	// Second connection should work with existing schema
	storage2, err := NewPostgresStorage(ctx)
	require.NoError(t, err)
	defer storage2.Close()

	// Should be able to store and retrieve
	err = storage2.Store(ctx, "inf-001", 100, `{"test": "data"}`, `{"response": "ok"}`)
	require.NoError(t, err)

	prompt, _, err := storage2.Retrieve(ctx, "inf-001", 100)
	require.NoError(t, err)
	assert.Equal(t, `{"test": "data"}`, prompt)
}

func TestPostgresStorage_IdempotentStore(t *testing.T) {
	cleanup, err := setupPostgresContainer(t)
	require.NoError(t, err)
	defer cleanup()

	ctx := context.Background()
	storage, err := NewPostgresStorage(ctx)
	require.NoError(t, err)
	defer storage.Close()

	// First store
	err = storage.Store(ctx, "inf-001", 100, `{"first": "value"}`, `{"response": "first"}`)
	require.NoError(t, err)

	// Second store with same ID should not error (ON CONFLICT DO NOTHING)
	err = storage.Store(ctx, "inf-001", 100, `{"second": "value"}`, `{"response": "second"}`)
	require.NoError(t, err)

	// Should still have first value
	prompt, _, err := storage.Retrieve(ctx, "inf-001", 100)
	require.NoError(t, err)
	assert.Equal(t, `{"first": "value"}`, prompt)
}

func TestHybridStorage_FallbackOnPGError(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()

	// Create file storage only (no PG)
	fileStorage := NewFileStorage(tempDir)

	// Store something in file storage
	err := fileStorage.Store(ctx, "inf-001", 100, `{"file": "data"}`, `{"file": "response"}`)
	require.NoError(t, err)

	// Now create hybrid with a broken PG connection
	// Since PGHOST is not set, NewPostgresStorage will fail
	// But we test the Retrieve fallback manually

	// Start postgres container
	cleanup, err := setupPostgresContainer(t)
	require.NoError(t, err)
	defer cleanup()

	pgStorage, err := NewPostgresStorage(ctx)
	require.NoError(t, err)
	defer pgStorage.Close()

	hybrid := NewHybridStorage(pgStorage, fileStorage)

	// Data not in PG, but is in file - should find it
	prompt, response, err := hybrid.Retrieve(ctx, "inf-001", 100)
	require.NoError(t, err)
	assert.Equal(t, `{"file": "data"}`, prompt)
	assert.Equal(t, `{"file": "response"}`, response)
}

func TestHybridStorage_PGPrimary(t *testing.T) {
	cleanup, err := setupPostgresContainer(t)
	require.NoError(t, err)
	defer cleanup()

	ctx := context.Background()
	tempDir := t.TempDir()

	pgStorage, err := NewPostgresStorage(ctx)
	require.NoError(t, err)
	defer pgStorage.Close()

	fileStorage := NewFileStorage(tempDir)
	hybrid := NewHybridStorage(pgStorage, fileStorage)

	// Store via hybrid (should go to PG)
	err = hybrid.Store(ctx, "inf-001", 100, `{"pg": "data"}`, `{"pg": "response"}`)
	require.NoError(t, err)

	// Retrieve should find it in PG
	prompt, response, err := hybrid.Retrieve(ctx, "inf-001", 100)
	require.NoError(t, err)
	assert.Equal(t, `{"pg": "data"}`, prompt)
	assert.Equal(t, `{"pg": "response"}`, response)

	// File storage should NOT have it
	_, _, err = fileStorage.Retrieve(ctx, "inf-001", 100)
	assert.ErrorIs(t, err, ErrNotFound)
}
