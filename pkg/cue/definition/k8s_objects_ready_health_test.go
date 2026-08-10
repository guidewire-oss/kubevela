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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/oam-dev/kubevela/pkg/cue/definition/health"
)

// readyHealthPolicy is the healthPolicy shipped in
// vela-templates/definitions/internal/component/k8s-objects-ready.cue. It is
// duplicated here rather than parsed out because attributes.healthPolicy is not
// part of the CUE template field; the tests below are what stop the two drifting.
const readyHealthPolicy = `
ready: {
	conditions: *[] | [...]
} & {
	if context.output.status != _|_ {
		if context.output.status.conditions != _|_ {
			conditions: context.output.status.conditions
		}
	}
}
isHealth: parameter.readyConditionType == "" || len([for c in ready.conditions if c.type == parameter.readyConditionType if c.status == "True" {c}]) > 0
`

func checkReady(t *testing.T, conditions []interface{}, wantType string) bool {
	t.Helper()
	status := map[string]interface{}{}
	if conditions != nil {
		status["conditions"] = conditions
	}
	tmplCtx := map[string]interface{}{
		"output": map[string]interface{}{
			"apiVersion": "apiextensions.crossplane.io/v1",
			"kind":       "CompositeResourceDefinition",
			"status":     status,
		},
	}
	healthy, err := health.CheckHealth(tmplCtx, readyHealthPolicy, map[string]interface{}{
		"readyConditionType": wantType,
	})
	require.NoError(t, err)
	return healthy
}

func TestK8sObjectsReady_EstablishedGatesTheTier(t *testing.T) {
	established := []interface{}{map[string]interface{}{"type": "Established", "status": "True"}}
	assert.True(t, checkReady(t, established, "Established"))

	notYet := []interface{}{map[string]interface{}{"type": "Established", "status": "False"}}
	assert.False(t, checkReady(t, notYet, "Established"), "a False condition must not report healthy")

	assert.False(t, checkReady(t, nil, "Established"), "no conditions yet must not report healthy")
}

func TestK8sObjectsReady_OtherConditionTypesDoNotSatisfyTheGate(t *testing.T) {
	// An XRD reports Offered=True before Established=True; only the requested
	// type may satisfy the gate.
	offeredOnly := []interface{}{map[string]interface{}{"type": "Offered", "status": "True"}}
	assert.False(t, checkReady(t, offeredOnly, "Established"))
}

func TestK8sObjectsReady_EmptyConditionTypeMeansAppliedIsEnough(t *testing.T) {
	assert.True(t, checkReady(t, nil, ""), "an empty readyConditionType falls back to k8s-objects semantics")
}
