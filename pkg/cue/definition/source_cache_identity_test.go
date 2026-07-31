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
	"strings"
	"testing"
)

// The generated storage.key covers the context a source reads. Properties are
// hashed here and appended, so a definition author cannot fail to discriminate on
// an input that changes the output - the failure that silently serves one binding
// another's value.
func TestCacheIdentity(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		props map[string]interface{}
		want  string // "" means: assert only via the relations below
	}{
		{
			name:  "no properties leaves the key alone",
			key:   "cluster-lookup-prod",
			props: nil,
			want:  "cluster-lookup-prod",
		},
		{
			name:  "an empty map is the same as none",
			key:   "cluster-lookup-prod",
			props: map[string]interface{}{},
			want:  "cluster-lookup-prod",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cacheIdentity(tc.key, tc.props)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestCacheIdentityDiscriminatesOnProperties(t *testing.T) {
	const key = "image-source"

	a, err := cacheIdentity(key, map[string]interface{}{"image": "nginx:1.25.0"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := cacheIdentity(key, map[string]interface{}{"image": "nginx:1.25.1"})
	if err != nil {
		t.Fatal(err)
	}

	if a == b {
		t.Fatal("two bindings with different properties must not share a cache entry")
	}
	if !strings.HasPrefix(a, key+"-") {
		t.Fatalf("the readable key must lead: got %q", a)
	}
	if err := ValidateCacheKey(a); err != nil {
		t.Fatalf("the assembled identity must be a legal object name: %v", err)
	}
}

func TestCacheIdentityIsStable(t *testing.T) {
	// Map iteration order must not leak into the hash, or the same binding would
	// address a different entry on each reconcile.
	props := map[string]interface{}{
		"zebra": "z", "alpha": "a", "middle": 3, "nested": map[string]interface{}{"b": 2, "a": 1},
	}
	first, err := cacheIdentity("k", props)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		again, err := cacheIdentity("k", props)
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("identity is not stable across calls: %q then %q", first, again)
		}
	}
}

func TestCacheIdentityOverflow(t *testing.T) {
	// The generated key is static text, so its resolved length is only knowable
	// here. An over-long identity is hashed rather than truncated: truncation
	// would let two distinct keys collide.
	long := strings.Repeat("a", MaxCacheKeyLen)

	got, err := cacheIdentity(long, map[string]interface{}{"x": "y"})
	if err != nil {
		t.Fatalf("an over-long key must be reduced, not rejected: %v", err)
	}
	if len(got) > MaxCacheKeyLen {
		t.Fatalf("identity is %d characters, over the %d limit", len(got), MaxCacheKeyLen)
	}
	if err := ValidateCacheKey(got); err != nil {
		t.Fatalf("the reduced identity must still be legal: %v", err)
	}

	// Distinct over-long inputs must stay distinct.
	other, err := cacheIdentity(long, map[string]interface{}{"x": "z"})
	if err != nil {
		t.Fatal(err)
	}
	if got == other {
		t.Fatal("reducing an over-long identity must not collapse distinct inputs")
	}
}
