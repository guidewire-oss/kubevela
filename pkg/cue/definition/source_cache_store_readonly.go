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
	"time"

	velaprocess "github.com/oam-dev/kubevela/pkg/cue/process"
)

// readOnlySourceCacheStore reads through to a real store and discards writes.
//
// It exists for the admission dry-run. That render resolves sources for real -
// it has to, to type-check the result against the consuming parameter - but it
// is a validation, and a validation must not leave anything behind.
//
// Writing from there was not a tidiness problem, it produced wrong metadata. The
// cache entry a real reconcile writes goes through the Config API store and is
// labelled with the ConfigTemplate name; the entry admission writes goes through
// the Secret store and is labelled with the source type. Whichever ran first
// won, because ApplySourceCacheMetadata is deliberately additive - so once
// admission started rendering these components, entries began appearing with the
// label the config factory does not expect.
//
// Reads still go through. Not reading would make every admission do the source's
// live I/O - a git fetch, an HTTP call - against a webhook timeout, and would
// have validation resolve different data than the render that follows it. The
// type check itself never depends on cached values: TypeOf builds sentinels from
// the schema, so what the cache holds cannot change what admission concludes.
type readOnlySourceCacheStore struct {
	delegate velaprocess.SourceCacheStore
}

// NewReadOnlySourceCacheStore wraps delegate so reads are served and writes are
// dropped. Returns nil if delegate is nil, matching the other constructors.
func NewReadOnlySourceCacheStore(delegate velaprocess.SourceCacheStore) velaprocess.SourceCacheStore {
	if delegate == nil {
		return nil
	}
	return &readOnlySourceCacheStore{delegate: delegate}
}

func (s *readOnlySourceCacheStore) Read(ctx context.Context, cacheKey string, ttl time.Duration) (map[string]interface{}, bool, bool, time.Time, error) {
	if s == nil || s.delegate == nil {
		return nil, false, false, time.Time{}, nil
	}
	return s.delegate.Read(ctx, cacheKey, ttl)
}

// Write is a no-op. A validation render resolves to check types, not to populate
// the cache the cluster shares.
func (s *readOnlySourceCacheStore) Write(_ context.Context, _, _ string, _ map[string]interface{}, _ velaprocess.SourceCacheWriteMeta) error {
	return nil
}

// Touch is a no-op for the same reason: last-accessed drives the GC sweep, and a
// validation is not a use.
func (s *readOnlySourceCacheStore) Touch(_ context.Context, _ string) error {
	return nil
}
