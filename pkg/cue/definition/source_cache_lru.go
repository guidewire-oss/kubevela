/*
Copyright 2026 The KubeVela Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package definition

import (
	"context"
	"sync"
	"time"

	"k8s.io/utils/lru"

	velaprocess "github.com/oam-dev/kubevela/pkg/cue/process"
)

const (
	// sourceCacheLRUSize bounds the number of distinct source cache keys held
	// in the process-level in-memory layer. Keys are the resolved storage.key
	// values, so this is the number of distinct (definition, cluster, params)
	// tuples cached in front of the persistent store.
	sourceCacheLRUSize = 512
	// sourceCacheLRUTTL is the in-memory freshness window for Layer 1. It is a
	// fixed implementation detail, independent of the per-source storageTTL
	// (Layer 2). The worst-case staleness for a running controller is therefore
	// storageTTL + sourceCacheLRUTTL; operators should account for this when
	// choosing storageTTL for time-sensitive sources.
	sourceCacheLRUTTL = 30 * time.Second
)

// sharedSourceLRU is the process-level Layer 1 cache. It is a singleton so
// entries survive across reconciles and are shared across all Applications that
// resolve to the same storage.key.
var sharedSourceLRU = lru.New(sourceCacheLRUSize)

type sourceLRUEntry struct {
	data      map[string]interface{}
	expiresAt time.Time
	// storeExpiresAt carries the persistent-store expiry so a Layer 1 hit can
	// report the same expiresAt callers would see from Layer 2.
	storeExpiresAt time.Time
	storedAt       time.Time
}

// lruSourceCacheStore wraps a persistent SourceCacheStore with a shared,
// process-level in-memory LRU (Layer 1). Reads consult the LRU first and only
// fall through to the wrapped store on a miss or in-memory expiry. Writes
// update both layers so a fresh resolution is immediately visible in-process.
type lruSourceCacheStore struct {
	delegate velaprocess.SourceCacheStore
	cache    *lru.Cache
	ttl      time.Duration
	mu       sync.Mutex
}

// NewLRUSourceCacheStore wraps delegate with the shared process-level LRU. If
// delegate is nil the wrapper is nil (source caching is disabled).
func NewLRUSourceCacheStore(delegate velaprocess.SourceCacheStore) velaprocess.SourceCacheStore {
	if delegate == nil {
		return nil
	}
	return &lruSourceCacheStore{
		delegate: delegate,
		cache:    sharedSourceLRU,
		ttl:      sourceCacheLRUTTL,
	}
}

// Read returns (data, stale, found, expiresAt, err). A Layer 1 hit within the
// in-memory TTL is always returned fresh (stale=false); the persistent store's
// own storageTTL freshness is only re-evaluated on a Layer 1 miss.
func (s *lruSourceCacheStore) Read(ctx context.Context, cacheKey string, ttl time.Duration) (map[string]interface{}, bool, bool, time.Time, error) {
	if s == nil || cacheKey == "" {
		return nil, false, false, time.Time{}, nil
	}
	if entry, ok := s.load(cacheKey); ok {
		return entry.data, false, true, entry.storeExpiresAt, nil
	}
	data, stale, found, expiresAt, err := s.delegate.Read(ctx, cacheKey, ttl)
	if err != nil {
		return data, stale, found, expiresAt, err
	}
	// Only promote fresh persistent hits into Layer 1. A stale Layer 2 value
	// must keep flowing through the resolver's refresh/onStaleFailure logic
	// rather than being masked as an in-memory hit.
	if found && !stale {
		s.store(cacheKey, data, expiresAt, s.effectiveTTL(ttl))
	}
	return data, stale, found, expiresAt, err
}

// Write persists through the delegate and, on success, refreshes Layer 1 so the
// new value is immediately visible in-process without a store round-trip.
func (s *lruSourceCacheStore) Write(ctx context.Context, cacheKey, sourceType string, data map[string]interface{}) error {
	if s == nil || cacheKey == "" {
		return nil
	}
	if err := s.delegate.Write(ctx, cacheKey, sourceType, data); err != nil {
		return err
	}
	// A just-written value is fresh for the whole in-memory window.
	s.store(cacheKey, data, time.Time{}, s.ttl)
	return nil
}

// effectiveTTL caps the in-memory freshness window at the caller's storageTTL so
// a short per-source TTL is not masked by the longer default Layer 1 window.
func (s *lruSourceCacheStore) effectiveTTL(storageTTL time.Duration) time.Duration {
	if storageTTL > 0 && storageTTL < s.ttl {
		return storageTTL
	}
	return s.ttl
}

func (s *lruSourceCacheStore) load(cacheKey string) (sourceLRUEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.cache.Get(cacheKey)
	if !ok {
		return sourceLRUEntry{}, false
	}
	entry, ok := v.(sourceLRUEntry)
	if !ok {
		s.cache.Remove(cacheKey)
		return sourceLRUEntry{}, false
	}
	if time.Now().After(entry.expiresAt) {
		s.cache.Remove(cacheKey)
		return sourceLRUEntry{}, false
	}
	return entry, true
}

func (s *lruSourceCacheStore) store(cacheKey string, data map[string]interface{}, storeExpiresAt time.Time, ttl time.Duration) {
	if ttl <= 0 {
		ttl = s.ttl
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache.Add(cacheKey, sourceLRUEntry{
		data:           data,
		expiresAt:      now.Add(ttl),
		storeExpiresAt: storeExpiresAt,
		storedAt:       now,
	})
}
