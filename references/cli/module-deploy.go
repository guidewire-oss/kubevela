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

package cli

import (
	"encoding/json"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	oamcommon "github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	pkgmodule "github.com/oam-dev/kubevela/pkg/module"
)

// moduleComponentType is the ComponentDefinition the deploy command builds a
// component of. Its template calls the module render service, which fetches the
// module and renders the owned Application holding the install tiers.
const moduleComponentType = "module"

// moduleComponentProperties are the parameters of the type: module component.
// It is a struct rather than a map so the rendered manifest has a stable field
// order.
type moduleComponentProperties struct {
	Module    string `json:"module"`
	Registry  string `json:"registry"`
	Namespace string `json:"namespace"`
}

// moduleDeployAppName is the name of the Application the deploy command
// creates. The "-deploy" suffix keeps it distinct from ownedModuleAppName: the
// render service names the owned Application "module-<name>", and the two
// collide when both live in the same namespace.
func moduleDeployAppName(moduleName string) string {
	return "module-" + moduleName + "-deploy"
}

// ownedModuleAppName is the name the render service gives the Application it
// renders for a module, mirroring RenderApplication in
// pkg/module/service/render.go.
func ownedModuleAppName(moduleName string) string {
	return "module-" + moduleName
}

// buildModuleApplication builds the one-component Application that installs a
// module. The registry name is the resolved one, not the raw flag, so the
// applied manifest records which registry was chosen.
func buildModuleApplication(moduleName, registryName, namespace string) (*v1beta1.Application, error) {
	props, err := json.Marshal(moduleComponentProperties{
		Module:    moduleName,
		Registry:  registryName,
		Namespace: namespace,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to encode the module component properties: %w", err)
	}
	return &v1beta1.Application{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1beta1.SchemeGroupVersion.String(),
			Kind:       v1beta1.ApplicationKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      moduleDeployAppName(moduleName),
			Namespace: namespace,
		},
		Spec: v1beta1.ApplicationSpec{
			Components: []oamcommon.ApplicationComponent{{
				Name:       moduleName,
				Type:       moduleComponentType,
				Properties: &runtime.RawExtension{Raw: props},
			}},
		},
	}, nil
}

// expectedModuleTiers returns the component names the render service will give
// the module's install tiers, in the order it emits them. It mirrors
// RenderApplication in pkg/module/service/render.go, so the status report can
// name every tier before the owned Application exists.
func expectedModuleTiers(mod *pkgmodule.Module) []string {
	if mod == nil {
		return nil
	}
	tiers := []string{}
	if mod.XRD != nil {
		tiers = append(tiers, mod.Name+"-xrd")
	}
	for _, apiVersion := range enabledModuleLines(mod) {
		line := mod.Lines[apiVersion]
		if line.Composition != nil {
			tiers = append(tiers, fmt.Sprintf("%s-%s-comp", mod.Name, apiVersion))
		}
		if len(line.Definitions) > 0 {
			tiers = append(tiers, fmt.Sprintf("%s-%s-defs", mod.Name, apiVersion))
		}
	}
	return tiers
}

// enabledModuleLines returns the module's enabled API versions, sorted
// lexically the way the render service sorts them.
func enabledModuleLines(mod *pkgmodule.Module) []string {
	out := make([]string, 0, len(mod.Lines))
	for apiVersion, line := range mod.Lines {
		if line.Enabled {
			out = append(out, apiVersion)
		}
	}
	sort.Strings(out)
	return out
}
