/*
Copyright 2021 The KubeVela Authors.

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

package oam

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The placement strings are part of the contract with module authors, who
// write them into annotations and _module.cue, so a change must be deliberate.
func TestPlacementConstants(t *testing.T) {
	assert.Equal(t, "resource.oam.dev/placement", AnnotationPlacement)
	assert.Equal(t, "hub", PlacementHub)
	assert.Equal(t, "runtime", PlacementRuntime)
}
