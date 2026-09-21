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

package utils

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/oam-dev/kubevela/pkg/definition/inherit"
)

// The shipped webservice template, read from the package that renders it.
//
// Validating against a real parent is the point of this test. A parent written
// for a test reads no `context`, and a compiler that never opens `context`
// therefore passes. Every shipped component definition does read it, so the
// only honest fixture is one of those: with `context` left unopened this test
// fails with "field not found: parameter", which is what a cluster reported
// before it was fixed, and which says nothing about the actual cause.
func webserviceTemplate(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../definition/inherit/testdata/webservice.cue")
	require.NoError(t, err)
	return string(b)
}

func TestValidateAgainstTheRealWebservice(t *testing.T) {
	// The abstracting shape: expose what this definition is about, decide the
	// rest of webservice on the user's behalf.
	child := `
super: properties: {
	image: parameter.image
	ports: [{port: 8080, expose: true}]
}

output: metadata: labels: "tenant.oam.dev/name": parameter.tenant

parameter: {
	image:  string
	tenant: string
}
`
	warnings, err := ValidateInheritedTemplate(context.Background(), "tenant-webservice", child,
		[]inherit.Level{{Name: "webservice", Template: webserviceTemplate(t)}}, inherit.ComponentSurface)

	require.NoError(t, err)
	require.Empty(t, warnings)
}

// webservice requires `image` and defaults everything else, so a child that
// forgets it is refused by name.
func TestRealWebserviceRequiresImage(t *testing.T) {
	child := `
super: properties: {ports: parameter.ports}
parameter: {tenant: string}
`
	_, err := ValidateInheritedTemplate(context.Background(), "tenant-webservice", child,
		[]inherit.Level{{Name: "webservice", Template: webserviceTemplate(t)}}, inherit.ComponentSurface)

	require.ErrorContains(t, err, `"image"`)
	require.ErrorContains(t, err, "gives no default")
}

func TestTemplateWithoutASuperCallIsRefusedAgainstTheRealWebservice(t *testing.T) {
	_, err := ValidateInheritedTemplate(context.Background(), "tenant-webservice",
		`output: kind: "ConfigMap"`,
		[]inherit.Level{{Name: "webservice", Template: webserviceTemplate(t)}}, inherit.ComponentSurface)

	require.ErrorContains(t, err, "never calls it")
}

// Inheriting webservice's whole parameter set while forwarding two fields of it
// publishes eighteen parameters that go nowhere. An application can set any of
// them and nothing happens, which was reproduced on a cluster: `replicas: 5`
// against a definition shaped like this produced one replica and no complaint.
func TestInheritingWebserviceSchemaWithoutForwardingItWarns(t *testing.T) {
	child := `
super: properties: {
	image: parameter.image
	ports: parameter.ports
}

output: metadata: labels: "tenant.oam.dev/name": parameter.tenant

parameter: super.parameter & {
	tenant: string
}
`
	warnings, err := ValidateInheritedTemplate(context.Background(), "tenant-webservice", child,
		[]inherit.Level{{Name: "webservice", Template: webserviceTemplate(t)}}, inherit.ComponentSurface)

	require.NoError(t, err, "worth saying, not worth refusing over")
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0], `"cpu"`)
	require.Contains(t, warnings[0], `"livenessProbe"`)
	require.Contains(t, warnings[0], "will do nothing")
	require.NotContains(t, warnings[0], `"image"`, "image is forwarded")
	require.NotContains(t, warnings[0], `"tenant"`, "tenant is used by the child")
}

// Forwarding the lot is how to inherit the lot, and draws no complaint.
func TestForwardingWebserviceParametersWholesaleIsClean(t *testing.T) {
	child := `
super: properties: parameter

output: metadata: labels: "tenant.oam.dev/name": parameter.tenant

parameter: super.parameter & {
	tenant: string
}
`
	warnings, err := ValidateInheritedTemplate(context.Background(), "tenant-webservice", child,
		[]inherit.Level{{Name: "webservice", Template: webserviceTemplate(t)}}, inherit.ComponentSurface)

	require.NoError(t, err)
	require.Empty(t, warnings)
}

// A conflict anywhere but the `super` block must be refused here, or it is
// admitted and first appears when an application renders the definition.
func TestConflictInTheChildsOwnTemplateIsRefused(t *testing.T) {
	child := `
super: properties: {image: parameter.image}

output: metadata: name: "a" & "b"

parameter: {image: string}
`
	_, err := ValidateInheritedTemplate(context.Background(), "tenant-webservice", child,
		[]inherit.Level{{Name: "webservice", Template: webserviceTemplate(t)}}, inherit.ComponentSurface)

	require.ErrorContains(t, err, "conflicting values")
}
