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

package application

import (
	"context"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/oam"
)

// addInheritedTypes adds the definitions each named type extends to the usage
// being permission-checked.
//
// The check covers the types an Application names, and extending one renders
// it, so a user's access to a parent is as much the question as their access to
// the child. The named type stays the field reported at fault, since that is
// what the Application says.
func (h *ValidatingHandler) addInheritedTypes(ctx context.Context, app *v1beta1.Application, usage *definitionUsage) {
	for typ, indices := range componentTypesOf(usage) {
		for _, parent := range h.ancestorNames(ctx, app.Namespace, typ, true) {
			usage.componentTypes[parent] = append(usage.componentTypes[parent], indices...)
		}
	}
	for typ, locations := range traitTypesOf(usage) {
		for _, parent := range h.ancestorNames(ctx, app.Namespace, typ, false) {
			usage.traitTypes[parent] = append(usage.traitTypes[parent], locations...)
		}
	}
}

// ancestorNames walks `extends` from a named type, in the Application's own
// namespace, which is the only place a parent may live.
//
// Anything it cannot resolve is skipped rather than reported. A type that does
// not exist, or a chain that loops, fails loudly elsewhere; refusing here would
// report it as a permission problem, which it is not.
func (h *ValidatingHandler) ancestorNames(ctx context.Context, namespace, typ string, component bool) []string {
	var names []string
	seen := map[string]bool{typ: true}

	for depth := 0; depth < maxInheritedTypes; depth++ {
		parent := h.extendsOf(ctx, namespace, typ, component)
		if parent == "" || seen[parent] {
			return names
		}
		seen[parent] = true
		names = append(names, parent)
		typ = parent
	}
	return names
}

// extendsOf reads spec.extends off one definition, or "" if there is none to
// read.
//
// It looks where the permission check itself looks, the Application's namespace
// and then the system one, since definitions normally live in vela-system.
func (h *ValidatingHandler) extendsOf(ctx context.Context, namespace, typ string, component bool) string {
	name := typ
	if i := strings.LastIndex(name, "@"); i > 0 {
		name = name[:i]
	}

	for _, ns := range []string{namespace, oam.SystemDefinitionNamespace} {
		key := client.ObjectKey{Namespace: ns, Name: name}
		if component {
			cd := &v1beta1.ComponentDefinition{}
			if err := h.Client.Get(ctx, key, cd); err == nil {
				return cd.Spec.Extends
			}
			continue
		}
		td := &v1beta1.TraitDefinition{}
		if err := h.Client.Get(ctx, key, td); err == nil {
			return td.Spec.Extends
		}
	}
	return ""
}

// maxInheritedTypes bounds the walk independently of the render path's own cap,
// so a chain that outgrows it costs a permission check rather than a loop.
const maxInheritedTypes = 10

// componentTypesOf and traitTypesOf copy the maps so they can be added to while
// being ranged over.
func componentTypesOf(usage *definitionUsage) map[string][]int {
	out := make(map[string][]int, len(usage.componentTypes))
	for k, v := range usage.componentTypes {
		out[k] = v
	}
	return out
}

func traitTypesOf(usage *definitionUsage) map[string][][2]int {
	out := make(map[string][][2]int, len(usage.traitTypes))
	for k, v := range usage.traitTypes {
		out[k] = v
	}
	return out
}
