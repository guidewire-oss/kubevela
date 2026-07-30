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
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/oam-dev/kubevela/pkg/definition"
)

// TestRepoSourceDefinitionsAreAccepted guards against the storage validator
// becoming stricter than the definitions actually shipped in this repo.
//
// It drives the real `vela def` conversion path (FromCUEString) so the string
// under test is exactly what lands in spec.schematic.cue.template, rather than a
// hand-sliced approximation of it.
func TestRepoSourceDefinitionsAreAccepted(t *testing.T) {
	const demoDefs = "../../../../../examples/source-definition-demo/definitions/*.cue"

	files, err := filepath.Glob(demoDefs)
	if err != nil {
		t.Fatalf("glob %s: %v", demoDefs, err)
	}
	if len(files) == 0 {
		t.Fatalf("no definitions matched %s — the path is stale, fix the glob rather than skipping", demoDefs)
	}

	checked := 0
	for _, f := range files {
		name := filepath.Base(f)
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		def := &definition.Definition{Unstructured: unstructured.Unstructured{Object: map[string]interface{}{}}}
		if err := def.FromCUEString(string(raw), nil); err != nil {
			t.Fatalf("%s: definition in-repo no longer converts: %v", name, err)
		}
		if def.GetKind() != "SourceDefinition" {
			continue // other definition types carry no storage: block
		}

		template, _, err := unstructured.NestedString(def.Object, "spec", "schematic", "cue", "template")
		if err != nil {
			t.Fatalf("%s: reading template: %v", name, err)
		}
		if err := ValidateSourceStorage(template); err != nil {
			t.Errorf("%s: rejected by ValidateSourceStorage: %v", name, err)
		}
		if err := ValidateSourceSchema(template); err != nil {
			t.Errorf("%s: rejected by ValidateSourceSchema: %v", name, err)
		}
		checked++
	}

	if checked == 0 {
		t.Fatal("no SourceDefinitions were checked — this test is no longer covering anything")
	}
}
