/*
Copyright 2022 The KubeVela Authors.

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

package multicluster

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"cuelang.org/go/cue"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	wfTypesv1alpha1 "github.com/kubevela/pkg/apis/oam/v1alpha1"

	apicommon "github.com/oam-dev/kubevela/apis/core.oam.dev/common"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1alpha1"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/appfile"
	"github.com/oam-dev/kubevela/pkg/multicluster"
	"github.com/oam-dev/kubevela/pkg/oam"
	oamprovidertypes "github.com/oam-dev/kubevela/pkg/workflow/providers/types"
)

func TestOverrideConfiguration(t *testing.T) {
	testCases := map[string]struct {
		Policies   []v1beta1.AppPolicy
		Components []apicommon.ApplicationComponent
		Outputs    []apicommon.ApplicationComponent
		Error      string
	}{
		"invalid-policies": {
			Policies: []v1beta1.AppPolicy{{
				Name:       "override-policy",
				Type:       "override",
				Properties: &runtime.RawExtension{Raw: []byte(`bad value`)},
			}},
			Error: "failed to parse override policy",
		},
		"empty-policy": {
			Policies: []v1beta1.AppPolicy{{
				Name:       "override-policy",
				Type:       "override",
				Properties: nil,
			}},
			Error: "empty properties",
		},
		"normal": {
			Policies: []v1beta1.AppPolicy{{
				Name:       "override-policy",
				Type:       "override",
				Properties: &runtime.RawExtension{Raw: []byte(`{"components":[{"name":"comp","properties":{"x":5}}]}`)},
			}},
			Components: []apicommon.ApplicationComponent{{
				Name:       "comp",
				Traits:     []apicommon.ApplicationTrait{},
				Properties: &runtime.RawExtension{Raw: []byte(`{"x":1}`)},
			}},
			Outputs: []apicommon.ApplicationComponent{{
				Name:       "comp",
				Traits:     []apicommon.ApplicationTrait{},
				Properties: &runtime.RawExtension{Raw: []byte(`{"x":5}`)},
			}},
		},
	}
	for name, tt := range testCases {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			comps, err := overrideConfiguration(tt.Policies, tt.Components)
			if tt.Error != "" {
				r.NotNil(err)
				r.Contains(err.Error(), tt.Error)
			} else {
				r.NoError(err)
				r.Equal(tt.Outputs, comps)
			}
		})
	}
}

func TestApplyComponentsDepends(t *testing.T) {
	r := require.New(t)
	const n, m = 50, 5
	var components []apicommon.ApplicationComponent
	var placements []v1alpha1.PlacementDecision
	for i := 0; i < n*3; i++ {
		comp := apicommon.ApplicationComponent{Name: fmt.Sprintf("comp-%d", i)}
		if i%3 != 0 {
			comp.DependsOn = append(comp.DependsOn, fmt.Sprintf("comp-%d", i-1))
		}
		if i%3 == 2 {
			comp.DependsOn = append(comp.DependsOn, fmt.Sprintf("comp-%d", i-1))
		}
		components = append(components, comp)
	}
	for i := 0; i < m; i++ {
		placements = append(placements, v1alpha1.PlacementDecision{Cluster: fmt.Sprintf("cluster-%d", i)})
	}

	applyMap := &sync.Map{}
	apply := func(_ context.Context, comp apicommon.ApplicationComponent, patcher *cue.Value, clusterName string, overrideNamespace string) (*unstructured.Unstructured, []*unstructured.Unstructured, bool, error) {
		time.Sleep(time.Duration(rand.Intn(200)+25) * time.Millisecond)
		applyMap.Store(fmt.Sprintf("%s/%s", clusterName, comp.Name), true)
		return nil, nil, true, nil
	}
	healthCheck := func(_ context.Context, comp apicommon.ApplicationComponent, patcher *cue.Value, clusterName string, overrideNamespace string) (bool, *apicommon.ApplicationComponentStatus, *unstructured.Unstructured, []*unstructured.Unstructured, error) {
		_, found := applyMap.Load(fmt.Sprintf("%s/%s", clusterName, comp.Name))
		return found, nil, nil, nil, nil
	}
	parallelism := 10

	countMap := func() int {
		cnt := 0
		applyMap.Range(func(key, value interface{}) bool {
			cnt++
			return true
		})
		return cnt
	}
	ctx := context.Background()
	healthy, _, err := applyComponents(ctx, apply, healthCheck, components, placements, parallelism)
	r.NoError(err)
	r.False(healthy)
	r.Equal(n*m, countMap())

	healthy, _, err = applyComponents(ctx, apply, healthCheck, components, placements, parallelism)
	r.NoError(err)
	r.False(healthy)
	r.Equal(2*n*m, countMap())

	healthy, _, err = applyComponents(ctx, apply, healthCheck, components, placements, parallelism)
	r.NoError(err)
	r.True(healthy)
	r.Equal(3*n*m, countMap())
}

func TestApplyComponentsIO(t *testing.T) {
	r := require.New(t)

	var (
		parallelism = 10
		applyMap    = new(sync.Map)
		ctx         = context.Background()
	)
	apply := func(_ context.Context, comp apicommon.ApplicationComponent, patcher *cue.Value, clusterName string, overrideNamespace string) (*unstructured.Unstructured, []*unstructured.Unstructured, bool, error) {
		time.Sleep(time.Duration(rand.Intn(200)+25) * time.Millisecond)
		applyMap.Store(fmt.Sprintf("%s/%s", clusterName, comp.Name), true)
		return nil, nil, true, nil
	}
	healthCheck := func(_ context.Context, comp apicommon.ApplicationComponent, patcher *cue.Value, clusterName string, overrideNamespace string) (bool, *apicommon.ApplicationComponentStatus, *unstructured.Unstructured, []*unstructured.Unstructured, error) {
		_, found := applyMap.Load(fmt.Sprintf("%s/%s", clusterName, comp.Name))
		return found, nil, &unstructured.Unstructured{Object: map[string]interface{}{
				"spec": map[string]interface{}{
					"path": fmt.Sprintf("%s/%s", clusterName, comp.Name),
				},
			}}, []*unstructured.Unstructured{
				{
					Object: map[string]interface{}{
						"metadata": map[string]interface{}{
							"labels": map[string]interface{}{
								oam.TraitResource: "obj",
							},
						},
						"spec": map[string]interface{}{
							"path": fmt.Sprintf("%s/%s", clusterName, comp.Name),
						},
					},
				},
			}, nil
	}

	resetStore := func() {
		applyMap = &sync.Map{}
	}
	countMap := func() int {
		cnt := 0
		applyMap.Range(func(key, value interface{}) bool {
			cnt++
			return true
		})
		return cnt
	}

	t.Run("apply components with io successfully", func(t *testing.T) {
		resetStore()
		const n, m = 10, 5
		var components []apicommon.ApplicationComponent
		var placements []v1alpha1.PlacementDecision
		for i := 0; i < n; i++ {
			comp := apicommon.ApplicationComponent{
				Name:       fmt.Sprintf("comp-%d", i),
				Properties: &runtime.RawExtension{Raw: []byte(fmt.Sprintf(`{"placeholder":%d}`, i))},
			}
			if i != 0 {
				comp.Inputs = wfTypesv1alpha1.StepInputs{
					{
						ParameterKey: "input_slot_1",
						From:         fmt.Sprintf("var-output-%d", i-1),
					},
					{
						ParameterKey: "input_slot_2",
						From:         fmt.Sprintf("var-outputs-%d", i-1),
					},
				}
			}
			if i != n-1 {
				comp.Outputs = wfTypesv1alpha1.StepOutputs{
					{
						ValueFrom: "output.spec.path",
						Name:      fmt.Sprintf("var-output-%d", i),
					},
					{
						ValueFrom: "outputs.obj.spec.path",
						Name:      fmt.Sprintf("var-outputs-%d", i),
					},
				}
			}
			components = append(components, comp)
		}
		for i := 0; i < m; i++ {
			placements = append(placements, v1alpha1.PlacementDecision{Cluster: fmt.Sprintf("cluster-%d", i)})
		}

		for i := 0; i < n; i++ {
			healthy, _, err := applyComponents(ctx, apply, healthCheck, components, placements, parallelism)
			r.NoError(err)
			r.Equal((i+1)*m, countMap())
			if i == n-1 {
				r.True(healthy)
			} else {
				r.False(healthy)
			}
		}
	})

	t.Run("apply components with io failed", func(t *testing.T) {
		resetStore()
		components := []apicommon.ApplicationComponent{
			{
				Name: "comp-0",
				Outputs: wfTypesv1alpha1.StepOutputs{
					{
						ValueFrom: "output.spec.error_path",
						Name:      "var1",
					},
				},
			},
			{
				Name: "comp-1",
				Inputs: wfTypesv1alpha1.StepInputs{
					{
						ParameterKey: "input_slot_1",
						From:         "var1",
					},
				},
			},
		}
		placements := []v1alpha1.PlacementDecision{
			{Cluster: "cluster-0"},
		}
		healthy, _, err := applyComponents(ctx, apply, healthCheck, components, placements, parallelism)
		r.NoError(err)
		r.False(healthy)
		healthy, _, err = applyComponents(ctx, apply, healthCheck, components, placements, parallelism)
		r.ErrorContains(err, "failed to lookup value")
		r.False(healthy)
	})

	t.Run("apply components with io and replication", func(t *testing.T) {
		// comp-0 ---> comp1-beijing  --> comp2-beijing
		// 		   |-> comp1-shanghai --> comp2-shanghai
		resetStore()
		storeKey := func(clusterName string, comp apicommon.ApplicationComponent) string {
			return fmt.Sprintf("%s/%s/%s", clusterName, comp.Name, comp.ReplicaKey)
		}
		type applyResult struct {
			output  *unstructured.Unstructured
			outputs []*unstructured.Unstructured
		}
		apply := func(_ context.Context, comp apicommon.ApplicationComponent, patcher *cue.Value, clusterName string, overrideNamespace string) (*unstructured.Unstructured, []*unstructured.Unstructured, bool, error) {
			time.Sleep(time.Duration(rand.Intn(200)+25) * time.Millisecond)
			key := storeKey(clusterName, comp)
			result := applyResult{
				output: &unstructured.Unstructured{Object: map[string]interface{}{
					"spec": map[string]interface{}{
						"path":        key,
						"anotherPath": key,
					},
				}}, outputs: []*unstructured.Unstructured{
					{
						Object: map[string]interface{}{
							"metadata": map[string]interface{}{
								"labels": map[string]interface{}{
									oam.TraitResource: "obj",
								},
							},
							"spec": map[string]interface{}{
								"path": key,
							},
						},
					},
				},
			}
			applyMap.Store(storeKey(clusterName, comp), result)
			return nil, nil, true, nil
		}
		healthCheck := func(_ context.Context, comp apicommon.ApplicationComponent, patcher *cue.Value, clusterName string, overrideNamespace string) (bool, *apicommon.ApplicationComponentStatus, *unstructured.Unstructured, []*unstructured.Unstructured, error) {
			key := storeKey(clusterName, comp)
			r, found := applyMap.Load(key)
			result, _ := r.(applyResult)
			return found, nil, result.output, result.outputs, nil
		}

		inputSlot := "input_slot"
		components := []apicommon.ApplicationComponent{
			{
				Name: "comp-0",
				Outputs: wfTypesv1alpha1.StepOutputs{
					{
						ValueFrom: "output.spec.path",
						Name:      "var1",
					},
				},
			},
			{
				Name: "comp-1",
				Inputs: wfTypesv1alpha1.StepInputs{
					{
						ParameterKey: inputSlot,
						From:         "var1",
					},
				},
				Outputs: wfTypesv1alpha1.StepOutputs{
					{
						ValueFrom: "output.spec.anotherPath",
						Name:      "var2",
					},
				},
				ReplicaKey: "beijing",
			},
			{
				Name: "comp-1",
				Inputs: wfTypesv1alpha1.StepInputs{
					{
						ParameterKey: inputSlot,
						From:         "var1",
					},
				},
				Outputs: wfTypesv1alpha1.StepOutputs{
					{
						ValueFrom: "output.spec.anotherPath",
						Name:      "var2",
					},
				},
				ReplicaKey: "shanghai",
			},
			{
				Name: "comp-2",
				Inputs: wfTypesv1alpha1.StepInputs{
					{
						ParameterKey: inputSlot,
						From:         "var2",
					},
				},
				ReplicaKey: "beijing",
			},
			{
				Name: "comp-2",
				Inputs: wfTypesv1alpha1.StepInputs{
					{
						ParameterKey: inputSlot,
						From:         "var2",
					},
				},
				ReplicaKey: "shanghai",
			},
		}
		placements := []v1alpha1.PlacementDecision{
			{Cluster: "cluster-0"},
		}
		healthy, _, err := applyComponents(ctx, apply, healthCheck, components, placements, parallelism)
		r.NoError(err)
		r.False(healthy)

		healthy, _, err = applyComponents(ctx, apply, healthCheck, components, placements, parallelism)
		r.NoError(err)
		r.False(healthy)

		healthy, _, err = applyComponents(ctx, apply, healthCheck, components, placements, parallelism)
		r.NoError(err)
		r.True(healthy)

	})
}

func TestLoadDispatcherTemplate(t *testing.T) {
	r := require.New(t)
	scheme := runtime.NewScheme()
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()
	dispatcher := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "core.oam.dev/v1beta1",
		"kind":       "Dispatcher",
		"metadata": map[string]interface{}{
			"name":      "default",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"schematic": map[string]interface{}{
				"cue": map[string]interface{}{
					"template": "targets: [{cluster: \"local\", namespace: \"default\"}]",
				},
			},
		},
	}}
	err := cli.Create(context.Background(), dispatcher)
	r.NoError(err)
	templates, err := loadDispatcherTemplates(context.Background(), cli, "default", "default")
	r.NoError(err)
	r.Contains(templates.TargetsTemplate, "targets")
}

func TestEvalDispatcherTemplate(t *testing.T) {
	r := require.New(t)
tpl := `
targets: [{cluster: "local", namespace: "demo"}]
output: {
	apiVersion: "v1"
	kind: "ConfigMap"
	metadata: {name: "cm"}
}
outputs: {
	svc: {
		apiVersion: "v1"
		kind: "Service"
		metadata: {name: "svc"}
	}
}
`
	val, err := compileDispatcherTemplate(context.Background(), tpl, map[string]interface{}{}, map[string]interface{}{})
	r.NoError(err)
	resolveVal := val.LookupPath(cue.ParsePath("targets"))
	r.True(resolveVal.Exists())
	var targets []v1alpha1.PlacementDecision
	bs, err := resolveVal.MarshalJSON()
	r.NoError(err)
	r.NoError(json.Unmarshal(bs, &targets))
	r.Len(targets, 1)
	r.Equal("local", targets[0].Cluster)
	outputVal := val.LookupPath(cue.ParsePath("output"))
	r.True(outputVal.Exists())
	outputJSON, err := outputVal.MarshalJSON()
	r.NoError(err)
	output := map[string]interface{}{}
	r.NoError(json.Unmarshal(outputJSON, &output))
	r.Equal("ConfigMap", output["kind"])
}

func TestResolveTargetsIgnoresDispatchOnlyContext(t *testing.T) {
	r := require.New(t)
	tpl := `
targets: [{cluster: "local", namespace: "demo"}]
output: context.output
`
	targets, err := callDispatcherResolveTargets(context.Background(), tpl, map[string]interface{}{}, map[string]interface{}{}, []v1alpha1.PlacementDecision{{Cluster: "local", Namespace: "demo"}})
	r.NoError(err)
	r.Len(targets, 1)
	r.Equal("local", targets[0].Cluster)
}

func TestResolveTargetsLegacyFieldStillWorks(t *testing.T) {
	r := require.New(t)
	tpl := `
resolveTargets: [{cluster: "local", namespace: "demo"}]
`
	targets, err := callDispatcherResolveTargets(context.Background(), tpl, map[string]interface{}{}, map[string]interface{}{}, []v1alpha1.PlacementDecision{{Cluster: "local", Namespace: "demo"}})
	r.NoError(err)
	r.Len(targets, 1)
	r.Equal("local", targets[0].Cluster)
}

func TestFilterComponents(t *testing.T) {
	r := require.New(t)
	all := []apicommon.ApplicationComponent{
		{Name: "a"},
		{Name: "b"},
	}
	filtered, err := filterComponents(all, []string{"b"})
	r.NoError(err)
	r.Len(filtered, 1)
	r.Equal("b", filtered[0].Name)

	_, err = filterComponents(all, []string{"c"})
	r.Error(err)
	r.Contains(err.Error(), "component(s) not found")
}

func TestResolveDispatcherStatusMapping(t *testing.T) {
	r := require.New(t)
	scheme := runtime.NewScheme()
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()

	dispatcher := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "core.oam.dev/v1beta1",
		"kind":       "Dispatcher",
		"metadata": map[string]interface{}{
			"name":      "status-only",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"schematic": map[string]interface{}{
				"cue": map[string]interface{}{
					"targetsTemplate":  `resolveTargets: []`,
					"dispatchTemplate": `output: {}`,
					"statusMappingTemplate": `
output: {
  status: {
    replicas: context.output.status.replicas
  }
}
outputs: {}
`,
				},
			},
		},
	}}
	r.NoError(cli.Create(context.Background(), dispatcher))

	mw := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "work.open-cluster-management.io/v1",
		"kind":       "ManifestWork",
		"metadata": map[string]interface{}{
			"name":      "vela-web-local",
			"namespace": "ocm-spoke",
		},
		"status": map[string]interface{}{
			"replicas": float64(2),
		},
	}}
	r.NoError(cli.Create(multicluster.ContextWithClusterName(context.Background(), "local"), mw))

	policies := []v1beta1.AppPolicy{{
		Name:       "ocm-topology",
		Type:       "ocm-topology",
		Properties: &runtime.RawExtension{Raw: []byte(`{"workNamespace":"ocm-spoke"}`)},
	}}
	mapped, err := ResolveDispatcherStatusMapping(
		context.Background(),
		cli,
		&appfile.Appfile{Name: "app", Namespace: "default"},
		"status-only",
		"local",
		v1alpha1.PlacementDecision{Cluster: "local"},
		apicommon.ApplicationComponent{Name: "web"},
		policies,
		[]*unstructured.Unstructured{mw},
	)
	r.NoError(err)
	r.NotNil(mapped)
	r.NotNil(mapped.Output)
	status, ok := mapped.Output["status"].(map[string]interface{})
	r.True(ok)
	r.Equal(float64(2), status["replicas"])
}

func TestDispatcherBaseContextInjectedIntoTemplates(t *testing.T) {
	r := require.New(t)
	base := buildDispatcherBaseContext(&appfile.Appfile{
		Name:            "app-with-ctx",
		Namespace:       "default",
		AppRevisionName: "app-with-ctx-v3",
		AppLabels: map[string]string{
			"team": "platform",
		},
		AppAnnotations: map[string]string{
			oam.AnnotationWorkflowName:  "rollout-v2",
			oam.AnnotationPublishVersion: "42",
			"example.io/marker":         "present",
		},
	}, "ocm-manifestwork", []map[string]interface{}{{"name": "topology"}})
	tpl := `
resolveTargets: [{
	cluster: context.placements[0].cluster
	namespace: context.namespace
}]
`
	targets, err := callDispatcherResolveTargets(
		context.Background(),
		tpl,
		nil,
		base,
		[]v1alpha1.PlacementDecision{{Cluster: "local", Namespace: "spoke"}},
	)
	r.NoError(err)
	r.Len(targets, 1)
	r.Equal("local", targets[0].Cluster)
	r.Equal("default", targets[0].Namespace)

	transformTpl := `
output: {
	apiVersion: "v1"
	kind: "ConfigMap"
	metadata: {
		name:      context.appName
		namespace: context.namespace
		labels: {
			dispatcher: context.dispatcher
			revision:   context.appRevision
			team:       context.appLabels.team
			workflow:   context.workflowName
			publish:    context.publishVersion
		}
		annotations: context.appAnnotations
	}
}
`
	result, err := callDispatcherTransform(
		context.Background(),
		transformTpl,
		nil,
		base,
		v1alpha1.PlacementDecision{Cluster: "local", Namespace: "spoke"},
		apicommon.ApplicationComponent{Name: "web"},
		&unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "apps/v1", "kind": "Deployment"}},
		nil,
	)
	r.NoError(err)
	r.Equal("ConfigMap", result.Output["kind"])
	metadata, ok := result.Output["metadata"].(map[string]interface{})
	r.True(ok)
	r.Equal("app-with-ctx", metadata["name"])
	r.Equal("default", metadata["namespace"])
	labels, ok := metadata["labels"].(map[string]interface{})
	r.True(ok)
	r.Equal("ocm-manifestwork", labels["dispatcher"])
	r.Equal("app-with-ctx-v3", labels["revision"])
	r.Equal("platform", labels["team"])
	r.Equal("rollout-v2", labels["workflow"])
	r.Equal("42", labels["publish"])
}

func TestApplyWithDispatcherHealthGating(t *testing.T) {
	r := require.New(t)
	scheme := runtime.NewScheme()
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()

	dispatcher := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "core.oam.dev/v1beta1",
		"kind":       "Dispatcher",
		"metadata": map[string]interface{}{
			"name":      "gate-test",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"schematic": map[string]interface{}{
				"cue": map[string]interface{}{
					"targetsTemplate":  `resolveTargets: []`,
					"dispatchTemplate": `output: context.output`,
				},
			},
		},
	}}
	r.NoError(cli.Create(context.Background(), dispatcher))

	componentRender := func(_ context.Context, comp apicommon.ApplicationComponent, _ *cue.Value, _ string, overrideNamespace string) (*unstructured.Unstructured, []*unstructured.Unstructured, error) {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]interface{}{
				"name":      comp.Name,
				"namespace": overrideNamespace,
			},
		}}, nil, nil
	}

	af := &appfile.Appfile{Name: "app", Namespace: "default"}
	components := []apicommon.ApplicationComponent{{Name: "web"}}
	placements := []v1alpha1.PlacementDecision{{Cluster: "local", Namespace: "default"}}

	t.Run("returns not healthy when health check is false", func(t *testing.T) {
		healthy, reason, err := applyWithDispatcher(
			context.Background(),
			cli,
			nil,
			componentRender,
			func(_ context.Context, _ apicommon.ApplicationComponent, _ *cue.Value, _ string, _ string) (bool, *apicommon.ApplicationComponentStatus, *unstructured.Unstructured, []*unstructured.Unstructured, error) {
				return false, nil, nil, nil, nil
			},
			"gate-test",
			af,
			nil,
			components,
			placements,
		)
		r.NoError(err)
		r.False(healthy)
		r.Contains(reason, "not healthy")
	})

	t.Run("returns error when health check errors", func(t *testing.T) {
		healthy, reason, err := applyWithDispatcher(
			context.Background(),
			cli,
			nil,
			componentRender,
			func(_ context.Context, _ apicommon.ApplicationComponent, _ *cue.Value, _ string, _ string) (bool, *apicommon.ApplicationComponentStatus, *unstructured.Unstructured, []*unstructured.Unstructured, error) {
				return false, nil, nil, nil, fmt.Errorf("boom")
			},
			"gate-test",
			af,
			nil,
			components,
			placements,
		)
		r.Error(err)
		r.False(healthy)
		r.Equal("", reason)
		r.Contains(err.Error(), "health check failed")
	})

	t.Run("returns healthy when health check passes", func(t *testing.T) {
		healthy, reason, err := applyWithDispatcher(
			context.Background(),
			cli,
			nil,
			componentRender,
			func(_ context.Context, _ apicommon.ApplicationComponent, _ *cue.Value, _ string, _ string) (bool, *apicommon.ApplicationComponentStatus, *unstructured.Unstructured, []*unstructured.Unstructured, error) {
				return true, nil, nil, nil, nil
			},
			"gate-test",
			af,
			nil,
			components,
			placements,
		)
		r.NoError(err)
		r.True(healthy)
		r.Equal("", reason)
	})
}

func TestApplyWithDispatcherHealthOverride(t *testing.T) {
	r := require.New(t)
	scheme := runtime.NewScheme()

	componentRender := func(_ context.Context, comp apicommon.ApplicationComponent, _ *cue.Value, _ string, overrideNamespace string) (*unstructured.Unstructured, []*unstructured.Unstructured, error) {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "work.open-cluster-management.io/v1",
			"kind":       "ManifestWork",
			"metadata": map[string]interface{}{
				"name":      comp.Name,
				"namespace": overrideNamespace,
			},
		}}, nil, nil
	}

	af := &appfile.Appfile{Name: "app", Namespace: "default"}
	components := []apicommon.ApplicationComponent{{Name: "web"}}
	placements := []v1alpha1.PlacementDecision{{Cluster: "local", Namespace: "default"}}

	t.Run("override unhealthy supersedes component health", func(t *testing.T) {
		cli := fake.NewClientBuilder().WithScheme(scheme).Build()
		dispatcher := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "core.oam.dev/v1beta1",
			"kind":       "Dispatcher",
			"metadata": map[string]interface{}{
				"name":      "override-unhealthy",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"schematic": map[string]interface{}{
					"cue": map[string]interface{}{
						"targetsTemplate":        `resolveTargets: []`,
						"dispatchTemplate":       `output: context.output`,
						"healthOverrideTemplate": `isHealth: false, message: "ocm apply signals not ready"`,
					},
				},
			},
		}}
		r.NoError(cli.Create(context.Background(), dispatcher))

		healthy, reason, err := applyWithDispatcher(
			context.Background(),
			cli,
			nil,
			componentRender,
			func(ctx context.Context, _ apicommon.ApplicationComponent, _ *cue.Value, _ string, _ string) (bool, *apicommon.ApplicationComponentStatus, *unstructured.Unstructured, []*unstructured.Unstructured, error) {
				if mapped := oamprovidertypes.DispatchMappedHealthFrom(ctx); mapped != nil && mapped.Healthy != nil {
					return *mapped.Healthy, nil, nil, nil, nil
				}
				return true, nil, nil, nil, nil
			},
			"override-unhealthy",
			af,
			nil,
			components,
			placements,
		)
		r.NoError(err)
		r.False(healthy)
		r.Contains(reason, "not healthy")
	})

	t.Run("override healthy bypasses component health check", func(t *testing.T) {
		cli := fake.NewClientBuilder().WithScheme(scheme).Build()
		dispatcher := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "core.oam.dev/v1beta1",
			"kind":       "Dispatcher",
			"metadata": map[string]interface{}{
				"name":      "override-healthy",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"schematic": map[string]interface{}{
					"cue": map[string]interface{}{
						"targetsTemplate":        `resolveTargets: []`,
						"dispatchTemplate":       `output: context.output`,
						"healthOverrideTemplate": `isHealth: true, message: "ocm conditions ready", details: {source: "override"}`,
					},
				},
			},
		}}
		r.NoError(cli.Create(context.Background(), dispatcher))

		healthy, reason, err := applyWithDispatcher(
			context.Background(),
			cli,
			nil,
			componentRender,
			func(ctx context.Context, _ apicommon.ApplicationComponent, _ *cue.Value, _ string, _ string) (bool, *apicommon.ApplicationComponentStatus, *unstructured.Unstructured, []*unstructured.Unstructured, error) {
				if mapped := oamprovidertypes.DispatchMappedHealthFrom(ctx); mapped != nil && mapped.Healthy != nil {
					return *mapped.Healthy, nil, nil, nil, nil
				}
				return false, nil, nil, nil, nil
			},
			"override-healthy",
			af,
			nil,
			components,
			placements,
		)
		r.NoError(err)
		r.True(healthy)
		r.Equal("", reason)
	})

	t.Run("override eval error keeps reconcile pending", func(t *testing.T) {
		cli := fake.NewClientBuilder().WithScheme(scheme).Build()
		dispatcher := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "core.oam.dev/v1beta1",
			"kind":       "Dispatcher",
			"metadata": map[string]interface{}{
				"name":      "override-bad-template",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"schematic": map[string]interface{}{
					"cue": map[string]interface{}{
						"targetsTemplate":        `resolveTargets: []`,
						"dispatchTemplate":       `output: context.output`,
						"healthOverrideTemplate": `isHealth: context.output.status..broken`,
					},
				},
			},
		}}
		r.NoError(cli.Create(context.Background(), dispatcher))

		healthy, reason, err := applyWithDispatcher(
			context.Background(),
			cli,
			nil,
			componentRender,
			func(_ context.Context, _ apicommon.ApplicationComponent, _ *cue.Value, _ string, _ string) (bool, *apicommon.ApplicationComponentStatus, *unstructured.Unstructured, []*unstructured.Unstructured, error) {
				return false, nil, nil, nil, nil
			},
			"override-bad-template",
			af,
			nil,
			components,
			placements,
		)
		r.NoError(err)
		r.False(healthy)
		r.Contains(reason, "health override pending")
	})
}
