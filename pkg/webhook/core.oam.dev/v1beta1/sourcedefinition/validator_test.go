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

package sourcedefinition

import (
	"strings"
	"testing"
)

func TestValidateSourceStorage(t *testing.T) {
	cases := []struct {
		name     string
		template string
		wantErr  string // substring; "" means the template must be accepted
	}{
		{
			name: "accept literal key",
			template: `
storage: {
  key: "cluster-config-reader"
}
output: { region: "us-east-1" }
`,
		},
		{
			name: "accept interpolated key",
			template: `
storage: {
  key:        "cluster-config-reader-\(context.cluster)"
  storageTTL: "15m"
}
output: { region: "us-east-1" }
`,
		},
		{
			name: "reject missing storage block",
			template: `
schema: { region: string }
output: { region: "us-east-1" }
`,
			wantErr: "must declare a storage: block",
		},
		{
			name: "reject storage without key",
			template: `
storage: {
  storageTTL: "15m"
}
`,
			wantErr: "must declare a key: field",
		},
		{
			name: "reject empty key",
			template: `
storage: {
  key: ""
}
`,
			wantErr: "must not be empty",
		},
		{
			name: "reject blank key",
			template: `
storage: {
  key: "   "
}
`,
			wantErr: "must not be empty",
		},
		{
			name: "reject uppercase in literal key",
			template: `
storage: {
  key: "Cluster-Config"
}
`,
			wantErr: "not allowed in a cache key",
		},
		{
			name: "reject illegal punctuation in literal key",
			template: `
storage: {
  key: "component:default/api"
}
`,
			wantErr: "not allowed in a cache key",
		},
		{
			name: "reject illegal literal segment of an interpolated key",
			template: `
storage: {
  key: "backstage:\(parameter.entityRef)"
}
`,
			wantErr: "literal segment",
		},
		{
			name: "reject non-string key",
			template: `
storage: {
  key: 42
}
`,
			wantErr: "must be a string",
		},
		{
			name:     "reject empty template",
			template: "   ",
			wantErr:  "must declare a cue template",
		},
		{
			name: "reject key over the length limit",
			template: `
storage: {
  key: "` + strings.Repeat("a", 254) + `"
}
`,
			wantErr: "exceeding the 253-character limit",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSourceStorage(tc.template)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("expected template to be accepted, got error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestValidateSourceSchema(t *testing.T) {
	cases := []struct {
		name     string
		template string
		wantErr  string // substring; "" means the template must be accepted
	}{
		{
			name: "accept a declared schema",
			template: `
schema: {
  region: string
}
storage: {key: "k"}
`,
		},
		{
			name: "accept optional and required fields",
			template: `
schema: {
  region:  string
  vpcId?:  string
  account!: string
}
`,
		},
		{
			name: "reject missing schema",
			template: `
storage: {key: "k"}
output: {region: "us-east-1"}
`,
			wantErr: "must declare a schema: block",
		},
		{
			name: "reject empty schema",
			template: `
schema: {}
storage: {key: "k"}
`,
			wantErr: "at least one field",
		},
		{
			name: "reject non-struct schema",
			template: `
schema: "not-a-struct"
`,
			wantErr: "must be a struct",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSourceSchema(tc.template)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("expected template to be accepted, got error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}
