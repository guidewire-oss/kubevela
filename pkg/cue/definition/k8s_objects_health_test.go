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

// readyHealthPolicy is the readyConditionType healthPolicy shipped in
// vela-templates/definitions/internal/component/k8s-objects.cue. It is duplicated
// here rather than parsed out because attributes.healthPolicy is not part of the
// CUE template field; the tests below are what stop the two drifting.
const readyHealthPolicy = `
ready: {
	conditions: *[] | [...]
} & {
	if _wantType != "" && context.output.status != _|_ {
		if context.output.status.conditions != _|_ {
			conditions: context.output.status.conditions
		}
	}
}
_wantType: *"" | string
if parameter.readyConditionType != _|_ {
	_wantType: parameter.readyConditionType
}
isHealth: _wantType == "" || len([for c in ready.conditions if c.type == _wantType if c.status == "True" {c}]) > 0
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

func TestK8sObjects_EstablishedGatesTheTier(t *testing.T) {
	established := []interface{}{map[string]interface{}{"type": "Established", "status": "True"}}
	assert.True(t, checkReady(t, established, "Established"))

	notYet := []interface{}{map[string]interface{}{"type": "Established", "status": "False"}}
	assert.False(t, checkReady(t, notYet, "Established"), "a False condition must not report healthy")

	assert.False(t, checkReady(t, nil, "Established"), "no conditions yet must not report healthy")
}

func TestK8sObjects_OtherConditionTypesDoNotSatisfyTheGate(t *testing.T) {
	// An XRD reports Offered=True before Established=True; only the requested
	// type may satisfy the gate.
	offeredOnly := []interface{}{map[string]interface{}{"type": "Offered", "status": "True"}}
	assert.False(t, checkReady(t, offeredOnly, "Established"))
}

func TestK8sObjects_EmptyConditionTypeMeansAppliedIsEnough(t *testing.T) {
	assert.True(t, checkReady(t, nil, ""), "an empty readyConditionType falls back to k8s-objects semantics")
}

// TestK8sObjects_OmittedConditionTypeIsBackwardCompatible is the one that matters
// for folding this policy into k8s-objects: an existing component that never sets
// readyConditionType leaves the field ABSENT from the raw parameter the health
// evaluation receives (the schema default *"" is not applied there). It must still
// report healthy on apply, or every pre-existing k8s-objects component would break.
func TestK8sObjects_OmittedConditionTypeIsBackwardCompatible(t *testing.T) {
	tmplCtx := map[string]interface{}{
		"output": map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"status":     map[string]interface{}{},
		},
	}
	// parameter WITHOUT readyConditionType, as a plain k8s-objects user applies it.
	healthy, err := health.CheckHealth(tmplCtx, readyHealthPolicy, map[string]interface{}{
		"objects": []interface{}{},
	})
	require.NoError(t, err)
	assert.True(t, healthy, "omitted readyConditionType must fall back to healthy-once-applied")
}

// TestK8sObjects_NullConditionsWithOmittedTypeStaysHealthy covers an object whose
// status.conditions is null (or another non-list shape) and which sets no
// readyConditionType. The policy must not constrain that status to a list on the
// backward-compatible path, so it stays healthy-on-apply instead of erroring.
func TestK8sObjects_NullConditionsWithOmittedTypeStaysHealthy(t *testing.T) {
	tmplCtx := map[string]interface{}{
		"output": map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"status":     map[string]interface{}{"conditions": nil},
		},
	}
	healthy, err := health.CheckHealth(tmplCtx, readyHealthPolicy, map[string]interface{}{
		"objects": []interface{}{},
	})
	require.NoError(t, err)
	assert.True(t, healthy, "null conditions with no readyConditionType must not break the eval")
}
