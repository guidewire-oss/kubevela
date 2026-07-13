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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	velaprocess "github.com/oam-dev/kubevela/pkg/cue/process"
)

type fakeSourceCacheStore struct {
	reads     int
	writes    int
	touches   int
	data      map[string]interface{}
	stale     bool
	found     bool
	expires   time.Time
	writeErr  error
	lastMeta  velaprocess.SourceCacheWriteMeta
	touchErr  error
	supportsT bool
}

func (f *fakeSourceCacheStore) Read(_ context.Context, _ string, _ time.Duration) (map[string]interface{}, bool, bool, time.Time, error) {
	f.reads++
	return f.data, f.stale, f.found, f.expires, nil
}

func (f *fakeSourceCacheStore) Write(_ context.Context, _, _ string, data map[string]interface{}, meta velaprocess.SourceCacheWriteMeta) error {
	f.writes++
	f.lastMeta = meta
	if f.writeErr != nil {
		return f.writeErr
	}
	f.data = data
	f.found = true
	f.stale = false
	return nil
}

func newTestLRUStore(delegate *fakeSourceCacheStore, ttl time.Duration) *lruSourceCacheStore {
	return &lruSourceCacheStore{
		delegate: delegate,
		cache:    sharedSourceLRU,
		ttl:      ttl,
	}
}

func TestLRUSourceCacheReadServesFreshFromMemory(t *testing.T) {
	sharedSourceLRU.Clear()
	delegate := &fakeSourceCacheStore{
		data:    map[string]interface{}{"region": "us-east-1"},
		found:   true,
		stale:   false,
		expires: time.Now().Add(time.Hour),
	}
	store := newTestLRUStore(delegate, time.Minute)

	// First read populates Layer 1 from the delegate.
	got, stale, found, _, err := store.Read(context.Background(), "key-a", time.Hour)
	assert.NoError(t, err)
	assert.True(t, found)
	assert.False(t, stale)
	assert.Equal(t, "us-east-1", got["region"])
	assert.Equal(t, 1, delegate.reads)

	// Second read is served from Layer 1; the delegate is not consulted again.
	got, stale, found, _, err = store.Read(context.Background(), "key-a", time.Hour)
	assert.NoError(t, err)
	assert.True(t, found)
	assert.False(t, stale)
	assert.Equal(t, "us-east-1", got["region"])
	assert.Equal(t, 1, delegate.reads, "delegate should not be read on Layer 1 hit")
}

func TestLRUSourceCacheInMemoryExpiryFallsThrough(t *testing.T) {
	sharedSourceLRU.Clear()
	delegate := &fakeSourceCacheStore{
		data:    map[string]interface{}{"region": "us-east-1"},
		found:   true,
		expires: time.Now().Add(time.Hour),
	}
	// Zero in-memory TTL means every entry is immediately expired.
	store := newTestLRUStore(delegate, 0)

	_, _, _, _, _ = store.Read(context.Background(), "key-b", time.Hour)
	_, _, _, _, _ = store.Read(context.Background(), "key-b", time.Hour)
	assert.Equal(t, 2, delegate.reads, "expired Layer 1 entry must fall through to delegate")
}

func TestLRUSourceCacheTTLCappedByStorageTTL(t *testing.T) {
	sharedSourceLRU.Clear()
	delegate := &fakeSourceCacheStore{
		data:    map[string]interface{}{"value": 1},
		found:   true,
		expires: time.Now().Add(time.Hour),
	}
	// Long in-memory window, but a tiny storageTTL passed by the caller: the
	// entry must expire per the storageTTL so a short-TTL source is not masked.
	store := newTestLRUStore(delegate, time.Hour)
	_, _, _, _, _ = store.Read(context.Background(), "key-c", time.Nanosecond)
	time.Sleep(time.Millisecond)
	_, _, _, _, _ = store.Read(context.Background(), "key-c", time.Nanosecond)
	assert.Equal(t, 2, delegate.reads, "storageTTL must cap the in-memory window")
}

func TestEffectiveTTL(t *testing.T) {
	s := &lruSourceCacheStore{ttl: 30 * time.Second}
	assert.Equal(t, 10*time.Second, s.effectiveTTL(10*time.Second), "shorter storageTTL wins")
	assert.Equal(t, 30*time.Second, s.effectiveTTL(time.Minute), "longer storageTTL capped at LRU ttl")
	assert.Equal(t, 30*time.Second, s.effectiveTTL(0), "zero storageTTL falls back to LRU ttl")
}

func TestLRUSourceCacheDoesNotPromoteStale(t *testing.T) {
	sharedSourceLRU.Clear()
	delegate := &fakeSourceCacheStore{
		data:    map[string]interface{}{"region": "old"},
		found:   true,
		stale:   true,
		expires: time.Now().Add(-time.Minute),
	}
	store := newTestLRUStore(delegate, time.Minute)

	_, stale, found, _, err := store.Read(context.Background(), "key-c", time.Hour)
	assert.NoError(t, err)
	assert.True(t, found)
	assert.True(t, stale)
	// Stale value must not be cached in Layer 1; a follow-up read hits delegate again.
	_, _, _, _, _ = store.Read(context.Background(), "key-c", time.Hour)
	assert.Equal(t, 2, delegate.reads, "stale delegate value must not be promoted to Layer 1")
}

func TestLRUSourceCacheWritePopulatesMemory(t *testing.T) {
	sharedSourceLRU.Clear()
	delegate := &fakeSourceCacheStore{}
	store := newTestLRUStore(delegate, time.Minute)

	err := store.Write(context.Background(), "key-d", "source-x", map[string]interface{}{"region": "eu-west-1"}, velaprocess.SourceCacheWriteMeta{TTL: time.Minute})
	assert.NoError(t, err)
	assert.Equal(t, 1, delegate.writes)
	assert.Equal(t, time.Minute, delegate.lastMeta.TTL)

	// A subsequent read is served from Layer 1 without touching the delegate.
	got, stale, found, _, err := store.Read(context.Background(), "key-d", time.Hour)
	assert.NoError(t, err)
	assert.True(t, found)
	assert.False(t, stale)
	assert.Equal(t, "eu-west-1", got["region"])
	assert.Equal(t, 0, delegate.reads, "write should populate Layer 1 so no delegate read is needed")
}

func TestLRUSourceCacheSharedAcrossStores(t *testing.T) {
	sharedSourceLRU.Clear()
	delegate1 := &fakeSourceCacheStore{
		data:    map[string]interface{}{"region": "shared"},
		found:   true,
		expires: time.Now().Add(time.Hour),
	}
	// Simulates a second Application's render in the same process.
	delegate2 := &fakeSourceCacheStore{
		data:    map[string]interface{}{"region": "should-not-be-read"},
		found:   true,
		expires: time.Now().Add(time.Hour),
	}
	store1 := newTestLRUStore(delegate1, time.Minute)
	store2 := newTestLRUStore(delegate2, time.Minute)

	_, _, _, _, _ = store1.Read(context.Background(), "shared-key", time.Hour)
	got, _, found, _, err := store2.Read(context.Background(), "shared-key", time.Hour)
	assert.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "shared", got["region"], "second store should hit the shared Layer 1 entry")
	assert.Equal(t, 0, delegate2.reads, "shared Layer 1 hit must avoid the second delegate")
}
