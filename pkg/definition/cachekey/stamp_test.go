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

package cachekey

import (
	"regexp"
	"strings"
	"testing"
)

// cue fmt aligns values within a struct, so an assertion on generated text has to
// ignore the padding rather than encode whichever alignment happened to apply.
var spaceRun = regexp.MustCompile(`[ \t]+`)

func normalise(s string) string { return spaceRun.ReplaceAllString(s, " ") }

func TestStamp(t *testing.T) {
	cases := []struct {
		name     string
		defName  string
		template string
		wantKey  string
		wantKeep []string // fragments that must survive stamping
		wantErr  string
	}{
		{
			name:    "adds a storage block when there is none",
			defName: "cluster-lookup",
			template: `
schema: {region: string}
output: {region: context.cluster}
`,
			wantKey:  `key: "cluster-lookup-\(context.cluster)"`,
			wantKeep: []string{"schema:", "output:", "context.cluster"},
		},
		{
			name:    "adds the key to an existing storage block, preserving its other fields",
			defName: "tenant-data",
			template: `
schema: {tenant: string}
storage: {
  storageTTL:     "15m"
  onStaleFailure: "fail"
}
output: {tenant: context.namespace}
`,
			wantKey:  `key: "tenant-data-\(context.namespace)"`,
			wantKeep: []string{`storageTTL:`, `"15m"`, `onStaleFailure:`, `"fail"`},
		},
		{
			// Regeneration must be idempotent, or every re-apply would show a diff.
			name:    "replaces a key that is already present",
			defName: "cluster-lookup",
			template: `
storage: {key: "stale-value", storageTTL: "15m"}
output: {region: context.cluster}
`,
			wantKey:  `key: "cluster-lookup-\(context.cluster)"`,
			wantKeep: []string{`storageTTL:`},
		},
		{
			name:    "a source reading no context gets a bare key",
			defName: "backstage-component",
			template: `
schema: {owner: string}
output: {owner: parameter.ref}
parameter: {ref: string}
`,
			wantKey: `key: "backstage-component"`,
		},
		{
			name:    "a forbidden read is reported rather than stamped",
			defName: "bad",
			template: `
output: {p: context.policyName}
`,
			wantErr: "policyName",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, hash, err := Stamp(tc.defName, tc.template)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error mentioning %q, got:\n%s", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected an error mentioning %q, got: %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if hash == "" {
				t.Fatal("expected the rules hash to be returned, so it can be recorded on the object")
			}
			if !strings.Contains(normalise(got), normalise(tc.wantKey)) {
				t.Fatalf("expected the template to contain:\n  %s\ngot:\n%s", tc.wantKey, got)
			}
			for _, keep := range tc.wantKeep {
				if !strings.Contains(normalise(got), normalise(keep)) {
					t.Fatalf("stamping dropped %q from the template:\n%s", keep, got)
				}
			}
		})
	}
}

// Stamping twice must not change the result, or a re-apply of an unchanged
// definition would show a diff and GitOps would never converge.
func TestStampIsIdempotent(t *testing.T) {
	const template = `
schema: {region: string}
storage: {storageTTL: "15m"}
output: {region: context.cluster, svc: context.appLabels["team"]}
`
	once, hash1, err := Stamp("cluster-lookup", template)
	if err != nil {
		t.Fatalf("first stamp: %v", err)
	}
	twice, hash2, err := Stamp("cluster-lookup", once)
	if err != nil {
		t.Fatalf("second stamp: %v", err)
	}
	if once != twice {
		t.Fatalf("stamping is not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
	if hash1 != hash2 {
		t.Fatalf("rules hash changed between stamps: %q then %q", hash1, hash2)
	}
}

// The stamped template must still be valid CUE - it is what the controller
// compiles at resolve time.
func TestStampedTemplateParses(t *testing.T) {
	const template = `
schema: {region: string}
storage: {storageTTL: "15m"}
output: {region: context.cluster}
`
	got, _, err := Stamp("cluster-lookup", template)
	if err != nil {
		t.Fatalf("stamping: %v", err)
	}
	rules, err := LoadRules()
	if err != nil {
		t.Fatalf("loading rules: %v", err)
	}
	// Re-inferring the stamped output must give the same dimensions: proof the
	// generated key did not become an input to its own regeneration.
	dims, err := Infer(got, rules)
	if err != nil {
		t.Fatalf("stamped template no longer infers: %v\n%s", err, got)
	}
	if len(dims) != 1 || dims[0].Field != "cluster" {
		t.Fatalf("expected [cluster] after stamping, got %v", dims)
	}
}
