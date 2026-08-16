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

package spokecluster

import (
	"context"
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	apitypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/multicluster"
	"github.com/oam-dev/kubevela/pkg/spokecluster/credential"
)

// cacheEpoch is a fixed instant the cache tests advance by hand. Nothing here sleeps: the
// point of the now seam is that refresh boundaries are assertable instantly.
var cacheEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// newTestCache builds a cache pinned to a caller-controlled clock. The store's sweeper
// goroutine is tied to a context cancelled when the spec ends, so specs do not leak one
// each.
func newTestCache(t GinkgoTInterface, now *time.Time) *credentialCache {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := newCredentialCache(ctx, DefaultCredentialCacheTTL)
	c.now = func() time.Time { return *now }
	return c
}

// cachedSpoke is a minimal SpokeCluster identity: the cache reads only the key, the UID
// and the generation.
func cachedSpoke(name string, generation int64, uid apitypes.UID) *v1beta1.SpokeCluster {
	sc := connectableSpoke(name)
	sc.Generation = generation
	sc.UID = uid
	return sc
}

// cacheableCredential is a credential the cache will hold: a token plus a refresh deadline.
func cacheableCredential(refreshAt time.Time) *credential.Materialized {
	return &credential.Materialized{
		Endpoint:    "https://spoke.example.com",
		CAData:      []byte("ca-bundle"),
		Token:       "token-value",
		NextRefresh: refreshAt,
	}
}

var _ = It("CredentialCacheHitAndExpiryBoundaries", func() {
	t := GinkgoT()
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		margin  time.Duration
		wantHit bool
	}{
		{name: "well inside the deadline", elapsed: time.Minute, margin: 0, wantHit: true},
		{name: "one tick before the deadline", elapsed: 10*time.Minute - time.Nanosecond, margin: 0, wantHit: true},
		{name: "exactly at the deadline", elapsed: 10 * time.Minute, margin: 0, wantHit: false},
		{name: "past the deadline", elapsed: 11 * time.Minute, margin: 0, wantHit: false},
		{name: "inside the deadline but inside the margin", elapsed: 9 * time.Minute, margin: 2 * time.Minute, wantHit: false},
		{name: "outside the margin", elapsed: 7 * time.Minute, margin: 2 * time.Minute, wantHit: true},
		{name: "margin lands exactly on the deadline", elapsed: 8 * time.Minute, margin: 2 * time.Minute, wantHit: false},
	} {
		By(tc.name, func() {
			now := cacheEpoch
			c := newTestCache(t, &now)
			sc := cachedSpoke("boundary", 1, "uid-1")

			c.Put(sc, cacheableCredential(now.Add(10*time.Minute)))
			now = now.Add(tc.elapsed)

			got, hit := c.Get(sc, tc.margin)
			if hit != tc.wantHit {
				t.Fatalf("hit = %v, want %v (elapsed %v, margin %v)", hit, tc.wantHit, tc.elapsed, tc.margin)
			}
			if hit && got == nil {
				t.Fatalf("hit reported but credential is nil")
			}
			if !hit && got != nil {
				t.Fatalf("miss reported but credential is %v", got)
			}
		})
	}
})

var _ = It("CredentialCacheRejectsStaticCredentials", func() {
	t := GinkgoT()
	now := cacheEpoch
	c := newTestCache(t, &now)
	sc := cachedSpoke("static", 1, "uid-1")

	// A zero NextRefresh means "do not cache", never "cache forever". Every kubeconfig
	// spoke lands here, and caching one would stop a rotated source Secret being picked up.
	c.Put(sc, &credential.Materialized{Endpoint: "https://spoke.example.com", Token: "static"})

	if _, hit := c.Get(sc, 0); hit {
		t.Fatalf("a credential with no refresh deadline must never be cached")
	}
})

var _ = It("CredentialCacheRejectsPastDueCredentials", func() {
	t := GinkgoT()
	now := cacheEpoch
	c := newTestCache(t, &now)
	sc := cachedSpoke("pastdue", 1, "uid-1")

	c.Put(sc, cacheableCredential(now.Add(-time.Second)))

	if _, hit := c.Get(sc, 0); hit {
		t.Fatalf("a credential already past its deadline must not be stored")
	}
})

var _ = It("CredentialCacheClampsDistantRefresh", func() {
	t := GinkgoT()
	now := cacheEpoch
	c := newTestCache(t, &now)
	sc := cachedSpoke("distant", 1, "uid-1")

	// A provider reporting a deadline a day out must not pin a credential for a day.
	c.Put(sc, cacheableCredential(now.Add(24*time.Hour)))

	now = now.Add(DefaultCredentialCacheTTL - time.Second)
	if _, hit := c.Get(sc, 0); !hit {
		t.Fatalf("expected a hit just inside the TTL clamp")
	}
	now = now.Add(2 * time.Second)
	if _, hit := c.Get(sc, 0); hit {
		t.Fatalf("expected a miss past the TTL clamp, whatever the provider reported")
	}
})

var _ = It("CredentialCacheInvalidatesOnGenerationChange", func() {
	t := GinkgoT()
	now := cacheEpoch
	c := newTestCache(t, &now)
	sc := cachedSpoke("generation", 1, "uid-1")

	c.Put(sc, cacheableCredential(now.Add(10*time.Minute)))
	if _, hit := c.Get(sc, 0); !hit {
		t.Fatalf("expected a hit for the generation the entry was stored at")
	}

	sc.Generation = 2
	if _, hit := c.Get(sc, 0); hit {
		t.Fatalf("a spec change must not reuse the credential derived from the old spec")
	}
})

var _ = It("CredentialCacheInvalidatesOnUIDChange", func() {
	t := GinkgoT()
	now := cacheEpoch
	c := newTestCache(t, &now)
	sc := cachedSpoke("recreated", 1, "uid-1")

	c.Put(sc, cacheableCredential(now.Add(10*time.Minute)))

	// Delete and recreate under the same name resets generation to 1, so generation alone
	// cannot tell the new object from the old one. The UID can.
	sc.UID = "uid-2"
	if _, hit := c.Get(sc, 0); hit {
		t.Fatalf("a recreated SpokeCluster must not inherit the previous object's credential")
	}
})

var _ = It("CredentialCacheHandsOutIndependentCopies", func() {
	t := GinkgoT()
	now := cacheEpoch
	c := newTestCache(t, &now)
	sc := cachedSpoke("copies", 1, "uid-1")

	source := cacheableCredential(now.Add(10 * time.Minute))
	source.ClientCertData = []byte("cert")
	source.ClientKeyData = []byte("key")
	c.Put(sc, source)

	By("mutating what the caller passed to Put", func() {
		source.CAData[0] = 'X'
		source.Endpoint = "https://mutated.example.com"
		got, hit := c.Get(sc, 0)
		if !hit {
			t.Fatalf("expected a hit")
		}
		if string(got.CAData) != "ca-bundle" || got.Endpoint != "https://spoke.example.com" {
			t.Fatalf("Put must store a copy; got CAData=%q endpoint=%q", got.CAData, got.Endpoint)
		}
	})

	By("mutating what a previous Get returned", func() {
		first, _ := c.Get(sc, 0)
		first.CAData[0] = 'Y'
		first.ClientKeyData[0] = 'Y'
		first.Endpoint = "https://mutated-again.example.com"

		second, _ := c.Get(sc, 0)
		if string(second.CAData) != "ca-bundle" || string(second.ClientKeyData) != "key" {
			t.Fatalf("Get must return a copy; got CAData=%q key=%q", second.CAData, second.ClientKeyData)
		}
		if second.Endpoint != "https://spoke.example.com" {
			t.Fatalf("endpoint = %q, want the stored value", second.Endpoint)
		}
	})
})

var _ = It("CredentialCacheInvalidateRemovesTheEntry", func() {
	t := GinkgoT()
	now := cacheEpoch
	c := newTestCache(t, &now)
	sc := cachedSpoke("invalidate", 1, "uid-1")

	c.Put(sc, cacheableCredential(now.Add(10*time.Minute)))
	c.Invalidate(client.ObjectKeyFromObject(sc))

	if _, hit := c.Get(sc, 0); hit {
		t.Fatalf("expected a miss after Invalidate")
	}
	// Idempotent, so every deletion path may call it unconditionally.
	c.Invalidate(client.ObjectKeyFromObject(sc))
})

var _ = It("NilCredentialCacheIsANoOpCache", func() {
	t := GinkgoT()
	var c *credentialCache
	sc := cachedSpoke("nil", 1, "uid-1")

	// A Reconciler built directly leaves this field nil, and must keep working.
	c.Put(sc, cacheableCredential(time.Now().Add(10*time.Minute)))
	c.Invalidate(client.ObjectKeyFromObject(sc))
	if _, hit := c.Get(sc, 0); hit {
		t.Fatalf("a nil cache must always miss")
	}
})

var _ = It("CredentialCacheTreatsAForeignEntryAsAMiss", func() {
	t := GinkgoT()
	now := cacheEpoch
	c := newTestCache(t, &now)
	sc := cachedSpoke("foreign", 1, "uid-1")

	// Nothing else writes this store, so a wrong type is a bug rather than a condition to
	// recover from. It must still not panic the reconcile loop.
	c.store.Put(client.ObjectKeyFromObject(sc), "not an entry", time.Hour)

	if _, hit := c.Get(sc, 0); hit {
		t.Fatalf("a value of the wrong type must read as a miss")
	}
})

var _ = It("CredentialCacheIsConcurrencySafe", func() {
	t := GinkgoT()
	now := cacheEpoch
	c := newTestCache(t, &now)
	cred := cacheableCredential(now.Add(10 * time.Minute))

	// Meaningful under -race. The assertion is only that it neither races nor deadlocks.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			shared := cachedSpoke("shared", 1, "uid-shared")
			private := cachedSpoke(fmt.Sprintf("private-%d", worker), 1, apitypes.UID(fmt.Sprintf("uid-%d", worker)))
			for j := 0; j < 200; j++ {
				c.Put(shared, cred)
				c.Put(private, cred)
				c.Get(shared, 0)
				c.Get(private, 0)
				c.Invalidate(client.ObjectKeyFromObject(shared))
			}
		}(i)
	}
	wg.Wait()
	_ = t
})

var _ = It("CredentialCacheIsDisabledByANonPositiveTTL", func() {
	t := GinkgoT()
	for _, ttl := range []time.Duration{0, -time.Minute} {
		By(fmt.Sprintf("ttl %v", ttl), func() {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			// The operator escape hatch behind --credential-cache-ttl=0. A nil cache is
			// already a working no-op, so disabling needs no separate code path.
			c := newCredentialCache(ctx, ttl)
			if c != nil {
				t.Fatalf("a non-positive ttl must yield a nil cache, got %v", c)
			}

			sc := cachedSpoke("disabled", 1, "uid-1")
			c.Put(sc, cacheableCredential(time.Now().Add(10*time.Minute)))
			if _, hit := c.Get(sc, 0); hit {
				t.Fatalf("a disabled cache must always miss")
			}
		})
	}
})

var _ = It("CredentialCacheHonoursAShortenedTTL", func() {
	t := GinkgoT()
	now := cacheEpoch
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := newCredentialCache(ctx, 2*time.Minute)
	c.now = func() time.Time { return now }
	sc := cachedSpoke("short-ttl", 1, "uid-1")

	// The provider reports ten minutes; the configured ttl is the stricter bound.
	c.Put(sc, cacheableCredential(now.Add(10*time.Minute)))

	now = now.Add(90 * time.Second)
	if _, hit := c.Get(sc, 0); !hit {
		t.Fatalf("expected a hit inside the configured ttl")
	}
	now = now.Add(time.Minute)
	if _, hit := c.Get(sc, 0); hit {
		t.Fatalf("expected a miss past the configured ttl, even though the provider deadline is further out")
	}
})

// A kubeconfig credential whose source Secret the controller does not watch must not
// be cached at all.
//
// The controller's Secret informer is scoped to the gateway namespace, and source
// kubeconfig Secrets in tenant namespaces are read through the uncached API reader,
// so no event is ever delivered for them. A JWT service-account token carries an exp
// claim, which gives the credential a non-zero NextRefresh and therefore made it
// cacheable: rotating such a Secret went unnoticed for up to the full TTL, with
// nothing in status or logs to say so. Caching only what the watch can invalidate is
// what closes that window.
var _ = It("CredentialCacheSkipsUnwatchedSourceSecrets", func() {
	t := GinkgoT()
	now := cacheEpoch
	c := newTestCache(t, &now)

	oldGatewayNS := multicluster.ClusterGatewaySecretNamespace
	multicluster.ClusterGatewaySecretNamespace = "vela-system"
	t.Cleanup(func() { multicluster.ClusterGatewaySecretNamespace = oldGatewayNS })

	for _, tc := range []struct {
		name      string
		mutate    func(*v1beta1.SpokeCluster)
		wantCache bool
		reason    string
	}{
		{
			name:      "kubeconfig in a tenant namespace",
			mutate:    func(sc *v1beta1.SpokeCluster) { sc.Spec.Credential.Kubeconfig.SecretRef.Namespace = "tenant-a" },
			wantCache: false,
			reason:    "no watch reaches a tenant namespace, so a rotate would go unnoticed until the TTL elapsed",
		},
		{
			name: "kubeconfig defaulting to the SpokeCluster's own tenant namespace",
			mutate: func(sc *v1beta1.SpokeCluster) {
				sc.Namespace = "tenant-b"
				sc.Spec.Credential.Kubeconfig.SecretRef.Namespace = ""
			},
			wantCache: false,
			reason:    "an empty secretRef.namespace means the spoke's own namespace, which is still unwatched",
		},
		{
			name: "kubeconfig in the gateway namespace",
			mutate: func(sc *v1beta1.SpokeCluster) {
				sc.Spec.Credential.Kubeconfig.SecretRef.Namespace = "vela-system"
			},
			wantCache: true,
			reason:    "the informer covers the gateway namespace, so the watch invalidates on rotate",
		},
		{
			name: "aws, which has no source Secret to go stale",
			mutate: func(sc *v1beta1.SpokeCluster) {
				sc.Spec.Credential = v1beta1.CredentialSpec{
					Type: v1beta1.CredentialTypeAWS,
					AWS: &v1beta1.AWSCredential{
						AuthMode: v1beta1.AWSAuthModePodIdentity, ClusterName: "spoke",
						Region: "us-west-2", RoleARN: "arn:aws:iam::1:role/r", ExternalID: "x",
					},
				}
			},
			wantCache: true,
			reason:    "the credential is minted from AWS, so nothing can change it behind the controller",
		},
	} {
		testCase := tc
		sc := cachedSpoke("unwatched-"+testCase.name, 1, apitypes.UID("uid-"+testCase.name))
		testCase.mutate(sc)
		c.Invalidate(client.ObjectKeyFromObject(sc))

		c.Put(sc, cacheableCredential(now.Add(10*time.Minute)))
		_, hit := c.Get(sc, 0)
		if hit != testCase.wantCache {
			t.Fatalf("%s: cached=%v, want %v: %s", testCase.name, hit, testCase.wantCache, testCase.reason)
		}
	}
})
