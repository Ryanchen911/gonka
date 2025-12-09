package payloadstorage

import (
	"context"
	"sync"
	"time"
)

const defaultMaxCacheSize = 1000

type cachedEntry struct {
	promptPayload   string
	responsePayload string
	expiresAt       time.Time
}

// CachedStorage wraps PayloadStorage with in-memory caching for Retrieve operations.
// Reduces disk I/O during validation bursts when multiple validators request same payload.
// Limited to ~1000 entries to prevent unbounded memory growth.
type CachedStorage struct {
	storage PayloadStorage
	mu      sync.RWMutex
	entries map[string]*cachedEntry
	ttl     time.Duration
	maxSize int
}

func NewCachedStorage(storage PayloadStorage, ttl time.Duration) *CachedStorage {
	return NewCachedStorageWithSize(storage, ttl, defaultMaxCacheSize)
}

func NewCachedStorageWithSize(storage PayloadStorage, ttl time.Duration, maxSize int) *CachedStorage {
	c := &CachedStorage{
		storage: storage,
		entries: make(map[string]*cachedEntry),
		ttl:     ttl,
		maxSize: maxSize,
	}
	go c.cleanupLoop()
	return c
}

func (c *CachedStorage) Store(ctx context.Context, inferenceId string, epochId uint64, promptPayload, responsePayload string) error {
	if err := c.storage.Store(ctx, inferenceId, epochId, promptPayload, responsePayload); err != nil {
		return err
	}

	c.mu.Lock()
	c.evictIfNeeded()
	c.entries[inferenceId] = &cachedEntry{
		promptPayload:   promptPayload,
		responsePayload: responsePayload,
		expiresAt:       time.Now().Add(c.ttl),
	}
	c.mu.Unlock()

	return nil
}

func (c *CachedStorage) Retrieve(ctx context.Context, inferenceId string, epochId uint64) (string, string, error) {
	c.mu.RLock()
	if cached, ok := c.entries[inferenceId]; ok && time.Now().Before(cached.expiresAt) {
		c.mu.RUnlock()
		return cached.promptPayload, cached.responsePayload, nil
	}
	c.mu.RUnlock()

	prompt, response, err := c.storage.Retrieve(ctx, inferenceId, epochId)
	if err != nil {
		return "", "", err
	}

	c.mu.Lock()
	c.evictIfNeeded()
	c.entries[inferenceId] = &cachedEntry{
		promptPayload:   prompt,
		responsePayload: response,
		expiresAt:       time.Now().Add(c.ttl),
	}
	c.mu.Unlock()

	return prompt, response, nil
}

func (c *CachedStorage) PruneEpoch(ctx context.Context, epochId uint64) error {
	return c.storage.PruneEpoch(ctx, epochId)
}

// evictIfNeeded removes entry closest to expiration if cache is at capacity.
// Must be called with write lock held.
func (c *CachedStorage) evictIfNeeded() {
	if len(c.entries) < c.maxSize {
		return
	}

	var oldestKey string
	var oldestTime time.Time
	first := true

	for key, entry := range c.entries {
		if first || entry.expiresAt.Before(oldestTime) {
			oldestKey = key
			oldestTime = entry.expiresAt
			first = false
		}
	}

	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

func (c *CachedStorage) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		c.cleanup()
	}
}

func (c *CachedStorage) cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for id, cached := range c.entries {
		if now.After(cached.expiresAt) {
			delete(c.entries, id)
		}
	}
}

var _ PayloadStorage = (*CachedStorage)(nil)
