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

package inherit

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// image is required, replicas has a default, tier is optional.
const contractParent = `
output: {
	kind: "Deployment"
	spec: replicas: parameter.replicas
}

parameter: {
	image:    string
	replicas: *1 | int
	tier?:    string
}
`

func check(t *testing.T, childTemplate string) ([]string, error) {
	t.Helper()
	return CheckCall(context.Background(), []Level{
		{Name: "tenant-webservice", Template: childTemplate},
		{Name: "webservice", Template: contractParent},
	}, ComponentSurface, testCompile())
}

func TestCallIsAcceptedWhenItFitsTheContract(t *testing.T) {
	warnings, err := check(t, `
super: properties: {image: parameter.image}
output: metadata: labels: tenant: parameter.tenant
parameter: {tenant: string}
`)
	require.NoError(t, err)
	require.Empty(t, warnings)
}

func TestTemplateThatNeverCallsSuperIsRefused(t *testing.T) {
	_, err := check(t, `
output: kind: "ConfigMap"
output: metadata: labels: tenant: parameter.tenant
parameter: {tenant: string}
`)
	require.ErrorContains(t, err, "never calls it")
	require.ErrorContains(t, err, "webservice")
}

func TestMissingRequiredParameterIsRefused(t *testing.T) {
	_, err := check(t, `
super: properties: {replicas: 3}
output: metadata: labels: tenant: parameter.tenant
parameter: {tenant: string}
`)
	require.ErrorContains(t, err, `"image"`)
	require.ErrorContains(t, err, "gives no default")
}

// A parameter with a default, or an optional one, is not something the child
// has to pass.
func TestDefaultedAndOptionalParametersAreNotRequired(t *testing.T) {
	_, err := check(t, `
super: properties: {image: parameter.image}
output: metadata: labels: tenant: parameter.tenant
parameter: {tenant: string}
`)
	require.NoError(t, err)
}

func TestWrongTypeIsRefused(t *testing.T) {
	_, err := check(t, `
super: properties: {image: 42}
output: metadata: labels: tenant: parameter.tenant
parameter: {tenant: string}
`)
	require.ErrorContains(t, err, "will not accept")
}

// An open parent takes names nobody declared, so an unexpected one is reported
// rather than refused.
func TestUnknownParameterWarnsOnAClosedParent(t *testing.T) {
	warnings, err := check(t, `
super: properties: {image: parameter.image, typo: "x"}
output: metadata: labels: tenant: parameter.tenant
parameter: {tenant: string}
`)
	require.NoError(t, err)
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0], `"typo"`)
	require.Contains(t, warnings[0], "does not declare")
}

// The labels trait declares `parameter: [string]: string`, so any name is a
// legitimate one and nothing should be reported.
func TestPatternConstrainedParentAcceptsAnyName(t *testing.T) {
	warnings, err := CheckCall(context.Background(), []Level{
		{Name: "tenant-labels", Template: `
super: properties: {"tenant.oam.dev/name": parameter.tenant}
output: metadata: labels: tenant: parameter.tenant
parameter: {tenant: string}
`},
		{Name: "labels", Template: `
patch: {
	metadata: labels: {for k, v in parameter {(k): v}}
}
parameter: [string]: string
`},
	}, TraitSurface, testCompile())

	require.NoError(t, err)
	require.Empty(t, warnings)
}

// Nothing to check when a definition extends nothing.
func TestChainOfOneIsNotChecked(t *testing.T) {
	warnings, err := CheckCall(context.Background(),
		[]Level{{Name: "webservice", Template: contractParent}}, ComponentSurface, testCompile())
	require.NoError(t, err)
	require.Empty(t, warnings)
}
