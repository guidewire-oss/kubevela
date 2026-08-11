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

package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/kubevela/pkg/util/singleton"

	"github.com/oam-dev/kubevela/apis/types"
	"github.com/oam-dev/kubevela/pkg/module"
	"github.com/oam-dev/kubevela/pkg/module/service/api"
	"github.com/oam-dev/kubevela/pkg/oam"
)

// rendererImpl fetches a module and renders its owned Application.
type rendererImpl struct {
	// fetchFn is a seam for tests. Production leaves it nil and the real
	// FetchModule is built per call, so no Kubernetes client is required at
	// construction time (init runs before the singletons are populated).
	fetchFn func(ctx context.Context, registry, moduleName string) (*module.Module, error)
}

// NewRenderer builds the module render service. It reads the Kubernetes client
// from the kubevela-pkg singleton at call time, not at construction, so a blank
// import can wire it during package init.
func NewRenderer() api.Renderer { return &rendererImpl{} }

// init registers the default renderer so a blank import of this package from
// cmd/core wires the module CueX provider, instead of injecting it in server.go.
func init() { api.SetDefaultRenderer(NewRenderer()) }

// fetch resolves the module through the injected seam (tests) or the real
// registry-backed FetchModule (production).
func (r *rendererImpl) fetch(ctx context.Context, registry, moduleName string) (*module.Module, error) {
	if r.fetchFn != nil {
		return r.fetchFn(ctx, registry, moduleName)
	}
	return NewService(module.NewStore(singleton.KubeClient.Get())).FetchModule(ctx, registry, moduleName)
}

// RenderModule fetches the named module and renders its owned Application.
func (r *rendererImpl) RenderModule(ctx context.Context, req api.ModuleRequest) (*api.ModuleResult, error) {
	mod, err := r.fetch(ctx, req.Registry, req.Module)
	if err != nil {
		return nil, err
	}
	app, err := RenderApplication(mod)
	if err != nil {
		return nil, err
	}
	return &api.ModuleResult{Application: app}, nil
}

// RenderApplication builds the module's owned Application. It is pure: given a
// parsed Module it touches no cluster and no registry, which is what lets the
// whole tier layout be unit-tested against a fixture.
func RenderApplication(mod *module.Module) (map[string]interface{}, error) {
	if mod == nil || mod.Name == "" {
		return nil, fmt.Errorf("render module: module has no name")
	}

	comps := []interface{}{}

	// The XRD tier is module-wide and gates every line beneath it.
	xrdTier := ""
	if mod.XRD != nil {
		xrdTier = mod.Name + "-xrd"
		comps = append(comps, readyTier(xrdTier, []interface{}{mod.XRD}, "Established", ""))
	}

	for _, apiVersion := range enabledLines(mod) {
		line := mod.Lines[apiVersion]

		// Each line hangs off the XRD, not off the previous line: lines are
		// siblings, so v2 must not wait on v1.
		dep := xrdTier
		if line.Composition != nil {
			compTier := fmt.Sprintf("%s-%s-comp", mod.Name, apiVersion)
			// Crossplane's Composition exposes no status.conditions, so there is
			// no readiness signal to wait on: an empty condition type means
			// healthy once applied. Kept as a readiness carrier so a real
			// condition is a one-line change if Crossplane ever adds one.
			comps = append(comps, readyTier(compTier, []interface{}{line.Composition}, "", dep))
			dep = compTier
		}

		if len(line.Definitions) == 0 {
			continue
		}
		defs := make([]interface{}, 0, len(line.Definitions))
		for _, def := range line.Definitions {
			defs = append(defs, stampIdentity(def, mod.Name, apiVersion))
		}
		comps = append(comps, objectsTier(fmt.Sprintf("%s-%s-defs", mod.Name, apiVersion), defs, dep))
	}

	return map[string]interface{}{
		"apiVersion": "core.oam.dev/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]interface{}{"name": "module-" + mod.Name},
		"spec":       map[string]interface{}{"components": comps},
	}, nil
}

// enabledLines returns the API versions to install: every line whose Enabled is
// true, sorted for a deterministic component order (and therefore a stable
// ApplicationRevision). Sorting is lexical, so v10 precedes v2; order between
// lines carries no meaning because lines are siblings under the XRD.
func enabledLines(mod *module.Module) []string {
	out := make([]string, 0, len(mod.Lines))
	for v, line := range mod.Lines {
		if !line.Enabled {
			continue
		}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// readyTier wraps objects in k8s-objects with a readyConditionType, so the next
// tier waits on a real status condition rather than on mere application. An empty
// readyConditionType means "healthy once applied" (plain k8s-objects behavior).
func readyTier(name string, objects []interface{}, readyConditionType, dependsOn string) map[string]interface{} {
	c := map[string]interface{}{
		"name": name,
		"type": "k8s-objects",
		"properties": map[string]interface{}{
			"objects":            objects,
			"readyConditionType": readyConditionType,
		},
	}
	if dependsOn != "" {
		c["dependsOn"] = []interface{}{dependsOn}
	}
	return c
}

// objectsTier wraps objects in a plain k8s-objects component (no readyConditionType,
// so healthy once applied). Nothing gates on the definitions tier.
func objectsTier(name string, objects []interface{}, dependsOn string) map[string]interface{} {
	c := map[string]interface{}{
		"name":       name,
		"type":       "k8s-objects",
		"properties": map[string]interface{}{"objects": objects},
	}
	if dependsOn != "" {
		c["dependsOn"] = []interface{}{dependsOn}
	}
	return c
}

// maxObjectNameLen is the Kubernetes limit for a metadata.name.
const maxObjectNameLen = 253

// stampIdentity returns a copy of def carrying its KEP-2.20 identity: the
// {module}-{apiVersion}-{name} object name, the definition identity labels, the
// full-name annotation, and the spec identity fields. It copies rather than
// mutates because the parsed Module is shared and may be cached.
func stampIdentity(def map[string]interface{}, moduleName, apiVersion string) map[string]interface{} {
	out := deepCopyMap(def)

	meta, _ := out["metadata"].(map[string]interface{})
	if meta == nil {
		meta = map[string]interface{}{}
		out["metadata"] = meta
	}
	shortName, _ := meta["name"].(string)

	fullName := fmt.Sprintf("%s-%s-%s", moduleName, apiVersion, shortName)
	meta["name"] = truncateName(fullName)

	labels, _ := meta["labels"].(map[string]interface{})
	if labels == nil {
		labels = map[string]interface{}{}
		meta["labels"] = labels
	}
	labels[types.LabelDefinitionModule] = moduleName
	labels[types.LabelDefinitionAPIVersion] = apiVersion
	labels[types.LabelDefinitionName] = shortName
	labels[oam.LabelAddonName] = moduleName

	annos, _ := meta["annotations"].(map[string]interface{})
	if annos == nil {
		annos = map[string]interface{}{}
		meta["annotations"] = annos
	}
	annos[types.AnnoDefinitionFullName] = fullName

	// These persist once GWCP-106678 adds the CRD identity fields. Until then
	// Kubernetes prunes them and the labels above carry identity.
	spec, _ := out["spec"].(map[string]interface{})
	if spec == nil {
		spec = map[string]interface{}{}
		out["spec"] = spec
	}
	spec["module"] = moduleName
	spec["apiVersion"] = apiVersion

	return out
}

// truncateName keeps name within the Kubernetes object-name limit, appending a
// stable 8-char digest of the full name so two long names that share a prefix
// still get distinct objects. The untruncated name lives on the full-name
// annotation.
func truncateName(name string) string {
	if len(name) <= maxObjectNameLen {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	return name[:maxObjectNameLen-len(suffix)] + suffix
}

// deepCopyMap copies nested maps and slices so stamping never writes through to
// the fetched Module.
func deepCopyMap(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		return deepCopyMap(t)
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, e := range t {
			out[i] = deepCopyValue(e)
		}
		return out
	case []map[string]interface{}:
		out := make([]interface{}, len(t))
		for i, e := range t {
			out[i] = deepCopyMap(e)
		}
		return out
	default:
		return v
	}
}
