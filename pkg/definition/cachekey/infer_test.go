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
	"strings"
	"testing"
)

// These cases are the frozen behaviour of the current rules. Changing them means
// changing what cache identity a definition gets, which invalidates every
// definition generated against the old rules - so a rules change must add a new
// rules file rather than edit this expectation.
func TestInfer(t *testing.T) {
	cases := []struct {
		name     string
		template string
		want     []string // "field" or "field[index]", in key order
		wantErr  string
	}{
		{
			name: "no context read is a definition-scoped key",
			template: `
schema: {region: string}
output: {region: parameter.region}
parameter: {region: string}
`,
			want: nil,
		},
		{
			name: "a single context read",
			template: `
schema: {region: string}
output: {region: context.cluster}
`,
			want: []string{"cluster"},
		},
		{
			// Declaration order in the template must not affect the key, or the
			// same definition formatted differently would cache differently.
			name: "dimensions are ordered by the rules, not by appearance",
			template: `
output: {
  a: context.name
  b: context.cluster
  c: context.appName
}
`,
			// name is identity, so it leads; the rest follow broad to narrow.
			want: []string{"name", "cluster", "appName"},
		},
		{
			// Scanning only output: would miss this, and the value would still
			// reach the output through the alias.
			name: "an aliased read still counts",
			template: `
_c: context.cluster
output: {region: _c}
`,
			want: []string{"cluster"},
		},
		{
			name: "an indexed read contributes that index",
			template: `
output: {svc: context.appLabels["example.org/service-name"]}
`,
			want: []string{`appLabels[example.org/service-name]`},
		},
		{
			// Two reads of the same map must assemble identically every time.
			name: "indexed reads are ordered by index",
			template: `
output: {
  b: context.appLabels["tier"]
  a: context.appLabels["team"]
}
`,
			want: []string{"appLabels[team]", "appLabels[tier]"},
		},
		{
			name: "the same field read twice contributes once",
			template: `
output: {
  a: context.cluster
  b: context.cluster
}
`,
			want: []string{"cluster"},
		},
		{
			// Over-inclusion is deliberate: a narrower cache is never wrong,
			// and deciding whether a read reaches output: is not tractable.
			name: "a read in errs: still counts",
			template: `
errs: [if context.cluster == "" {"no cluster"}]
output: {region: "us-east-1"}
`,
			want: []string{"cluster"},
		},
		{
			// A source's output must not depend on which consumer asked, so the
			// component's identity is not readable. One that genuinely varies per
			// component takes it as a property.
			name: "consumer identity is not readable",
			template: `
output: {t: context.componentType}
`,
			wantErr: "componentType",
		},
		{
			name: "policy context is rejected",
			template: `
output: {p: context.policyName}
`,
			wantErr: "policyName",
		},
		{
			name: "internal plumbing is rejected",
			template: `
output: {p: context.appSourceCacheStore}
`,
			wantErr: "appSourceCacheStore",
		},
		{
			// Fail closed: a context field added later must not silently become
			// an unkeyed dependency.
			name: "an unclassified field is rejected",
			template: `
output: {p: context.somethingNew}
`,
			wantErr: "somethingNew",
		},
		{
			name: "a dynamic index is rejected",
			template: `
parameter: {k: string}
output: {p: context.appLabels[parameter.k]}
`,
			wantErr: "index",
		},
		{
			// storage.key is generated from the reads, so it cannot also be one
			// of them. Scanning it would make a regenerated key depend on the
			// previous key rather than on the resolution logic.
			name: "an existing storage.key is not itself a read",
			template: `
storage: {
  key:        "old-\(context.appName)"
  storageTTL: "15m"
}
output: {region: context.cluster}
`,
			want: []string{"cluster"},
		},
		{
			// Only key is generated; the rest of storage: is authored and can
			// legitimately depend on context.
			name: "the rest of storage: is still scanned",
			template: `
storage: {
  key:        "old-\(context.appName)"
  storageTTL: context.appLabels["ttl"]
}
output: {region: "us-east-1"}
`,
			want: []string{"appLabels[ttl]"},
		},
		{
			name:     "an unparseable template is an error",
			template: `output: {`,
			wantErr:  "parse",
		},
	}

	rules, err := LoadRules()
	if err != nil {
		t.Fatalf("loading embedded rules: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Infer(tc.template, rules)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error mentioning %q, got dimensions %v", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected an error mentioning %q, got: %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var names []string
			for _, d := range got {
				names = append(names, d.String())
			}
			if len(names) != len(tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, names)
			}
			for i := range names {
				if names[i] != tc.want[i] {
					t.Fatalf("expected %v, got %v", tc.want, names)
				}
			}
		})
	}
}

// The hash identifies the policy, not the file. Editing a comment or the declared
// version must not change it - that would force every stamped definition to be
// regenerated for no behavioural reason. A change to what is readable, or to the
// order it contributes in, must.
func TestPolicyHashCoversBehaviourOnly(t *testing.T) {
	base, err := LoadRules()
	if err != nil {
		t.Fatalf("loading rules: %v", err)
	}

	prose := &Rules{Version: "something else entirely", keyed: base.keyed}
	proseHash, err := prose.policyHash()
	if err != nil {
		t.Fatal(err)
	}
	if proseHash != base.Hash {
		t.Errorf("renaming the version must not change the identity: %q became %q", base.Hash, proseHash)
	}

	// One more readable field.
	added := &Rules{keyed: map[string]keyedField{}}
	for f, e := range base.keyed {
		added.keyed[f] = e
	}
	added.keyed["somethingNew"] = keyedField{Order: 99}
	addedHash, err := added.policyHash()
	if err != nil {
		t.Fatal(err)
	}
	if addedHash == base.Hash {
		t.Error("making another field readable must change the identity")
	}

	// Same fields, different order.
	reordered := &Rules{keyed: map[string]keyedField{}}
	for f, e := range base.keyed {
		e.Order += 100
		reordered.keyed[f] = e
	}
	reorderedHash, err := reordered.policyHash()
	if err != nil {
		t.Fatal(err)
	}
	if reorderedHash == base.Hash {
		t.Error("reordering the key segments must change the identity")
	}
}

// Everything outside the keyed list gets the same rejection, naming the field and
// pointing at properties.
func TestUnsupportedContextIsOneMessage(t *testing.T) {
	rules, err := LoadRules()
	if err != nil {
		t.Fatalf("loading rules: %v", err)
	}
	for _, field := range []string{"policyName", "appSourceCacheStore", "componentType", "somethingNobodyHasAddedYet"} {
		_, err := Infer("output: {p: context."+field+"}\n", rules)
		if err == nil {
			t.Errorf("context.%s should be unsupported", field)
			continue
		}
		if !strings.Contains(err.Error(), "context."+field) {
			t.Errorf("rejection should name the field; got: %v", err)
		}
		if !strings.Contains(err.Error(), "properties") {
			t.Errorf("rejection should point at properties; got: %v", err)
		}
	}
}
