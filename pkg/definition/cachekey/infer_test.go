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
	"os"
	"regexp"
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
			want: []string{"cluster", "appName", "name"},
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
			name: "consumer identity is keyed, sorted last",
			template: `
output: {
  a: context.componentType
  b: context.cluster
  c: context.name
}
`,
			want: []string{"cluster", "name", "componentType"},
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

// Every context keyword must be classified. Without this, adding a field to
// keyword.go and forgetting the rules file would make it silently unkeyed - the
// failure that serves one consumer another's data.
func TestEveryContextKeywordIsClassified(t *testing.T) {
	rules, err := LoadRules()
	if err != nil {
		t.Fatalf("loading embedded rules: %v", err)
	}

	// Read the constants rather than a list maintained here, so the source of
	// truth cannot drift from its copy.
	const keywordFile = "../../cue/process/keyword.go"
	raw, err := os.ReadFile(keywordFile)
	if err != nil {
		t.Fatalf("reading %s: %v", keywordFile, err)
	}
	re := regexp.MustCompile(`Context[A-Za-z]+\s*=\s*"([a-zA-Z]+)"`)
	matches := re.FindAllStringSubmatch(string(raw), -1)
	if len(matches) == 0 {
		t.Fatalf("found no context keywords in %s — the scan is broken, not the rules", keywordFile)
	}

	var unclassified []string
	for _, m := range matches {
		if !rules.IsClassified(m[1]) {
			unclassified = append(unclassified, m[1])
		}
	}
	if len(unclassified) > 0 {
		t.Errorf("context keywords with no classification: %v\n"+
			"Add each to keyed or forbidden in the rules file. An unclassified field is "+
			"rejected in a source template, so leaving it out silently blocks definitions "+
			"that read it — and worse, a field meant to be keyed becomes an unkeyed dependency.", unclassified)
	}
}
