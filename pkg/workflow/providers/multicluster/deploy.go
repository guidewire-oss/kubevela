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
	"strings"
	"unicode"
	"sync"

	"cuelang.org/go/cue"
	cueerrors "cuelang.org/go/cue/errors"
	"cuelang.org/go/cue/cuecontext"
	"github.com/kubevela/pkg/cue/cuex"
	pkgmaps "github.com/kubevela/pkg/util/maps"
	"github.com/kubevela/pkg/util/slices"
	"github.com/kubevela/workflow/pkg/cue/model/value"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workflowerrors "github.com/kubevela/workflow/pkg/errors"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1alpha1"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/apis/types"
	"github.com/oam-dev/kubevela/pkg/appfile"
	"github.com/oam-dev/kubevela/pkg/multicluster"
	"github.com/oam-dev/kubevela/pkg/oam"
	pkgpolicy "github.com/oam-dev/kubevela/pkg/policy"
	"github.com/oam-dev/kubevela/pkg/policy/envbinding"
	"github.com/oam-dev/kubevela/pkg/resourcekeeper"
	"github.com/oam-dev/kubevela/pkg/utils"
	velaerrors "github.com/oam-dev/kubevela/pkg/utils/errors"
	"github.com/oam-dev/kubevela/pkg/workflow/dispatchruntime"
	oamprovidertypes "github.com/oam-dev/kubevela/pkg/workflow/providers/types"
	wfprovidertypes "github.com/kubevela/workflow/pkg/providers/types"
)

// DeployParameter is the parameter of deploy workflow step
type DeployParameter struct {
	// Declare the policies that used for this deployment. If not specified, the components will be deployed to the hub cluster.
	Policies []string `json:"policies,omitempty"`
	// Maximum number of concurrent delivered components.
	Parallelism int64 `json:"parallelism"`
	// If set false, this step will apply the components with the terraform workload.
	IgnoreTerraformComponent bool `json:"ignoreTerraformComponent"`
	// The policies that embeds in the `deploy` step directly
	InlinePolicies []v1beta1.AppPolicy `json:"inlinePolicies,omitempty"`
	// Components optionally limits deploy to the named components only.
	Components []string `json:"components,omitempty"`
	// Dispatcher is the Dispatcher CRD name that controls resource transformation before dispatch.
	Dispatcher string `json:"dispatcher,omitempty"`
}

// DefaultDispatcher is applied when deploy step does not set `dispatcher`.
// It is configured by controller-level flag `--default-dispatcher`.
var DefaultDispatcher string

// DeployWorkflowStepExecutor executor to run deploy workflow step
type DeployWorkflowStepExecutor interface {
	Deploy(ctx context.Context) (healthy bool, reason string, err error)
}

// NewDeployWorkflowStepExecutor .
func NewDeployWorkflowStepExecutor(cli client.Client, af *appfile.Appfile, apply oamprovidertypes.ComponentApply, componentRender oamprovidertypes.ComponentRender, healthCheck oamprovidertypes.ComponentHealthCheck, renderer oamprovidertypes.WorkloadRender, parameter DeployParameter) DeployWorkflowStepExecutor {
	return &deployWorkflowStepExecutor{
		cli:             cli,
		af:              af,
		apply:           apply,
		componentRender: componentRender,
		healthCheck:     healthCheck,
		renderer:        renderer,
		parameter:       parameter,
	}
}

func NewDeployWorkflowStepExecutorWithKubeHandlers(cli client.Client, kubeHandlers *wfprovidertypes.KubeHandlers, af *appfile.Appfile, apply oamprovidertypes.ComponentApply, componentRender oamprovidertypes.ComponentRender, healthCheck oamprovidertypes.ComponentHealthCheck, renderer oamprovidertypes.WorkloadRender, parameter DeployParameter) DeployWorkflowStepExecutor {
	return &deployWorkflowStepExecutor{
		cli:             cli,
		kubeHandlers:    kubeHandlers,
		af:              af,
		apply:           apply,
		componentRender: componentRender,
		healthCheck:     healthCheck,
		renderer:        renderer,
		parameter:       parameter,
	}
}

type deployWorkflowStepExecutor struct {
	cli             client.Client
	kubeHandlers    *wfprovidertypes.KubeHandlers
	af              *appfile.Appfile
	apply           oamprovidertypes.ComponentApply
	componentRender oamprovidertypes.ComponentRender
	healthCheck     oamprovidertypes.ComponentHealthCheck
	renderer        oamprovidertypes.WorkloadRender
	parameter       DeployParameter
}

// Deploy execute deploy workflow step
func (executor *deployWorkflowStepExecutor) Deploy(ctx context.Context) (bool, string, error) {
	policies, err := selectPolicies(executor.af.Policies, executor.parameter.Policies)
	if err != nil {
		return false, "", err
	}
	policies = append(policies, fillInlinePolicyNames(executor.parameter.InlinePolicies)...)
	components, err := loadComponents(ctx, executor.renderer, executor.cli, executor.af, executor.af.Components, executor.parameter.IgnoreTerraformComponent)
	if err != nil {
		return false, "", err
	}
	components, err = filterComponents(components, executor.parameter.Components)
	if err != nil {
		return false, "", err
	}

	// Dealing with topology, override and replication policies in order.
	placements, err := pkgpolicy.GetPlacementsFromTopologyPolicies(ctx, executor.cli, executor.af.Namespace, policies, resourcekeeper.AllowCrossNamespaceResource)
	if err != nil {
		return false, "", err
	}
	components, err = overrideConfiguration(policies, components)
	if err != nil {
		return false, "", err
	}
	components, err = pkgpolicy.ReplicateComponents(policies, components)
	if err != nil {
		return false, "", err
	}
	dispatcherName := executor.parameter.Dispatcher
	if dispatcherName == "" {
		dispatcherName = DefaultDispatcher
	}
	if dispatcherName != "" {
		return applyWithDispatcher(ctx, executor.cli, executor.kubeHandlers, executor.componentRender, executor.healthCheck, dispatcherName, executor.af, policies, components, placements)
	}
	return applyComponents(ctx, executor.apply, executor.healthCheck, components, placements, int(executor.parameter.Parallelism))
}

func filterComponents(all []common.ApplicationComponent, selected []string) ([]common.ApplicationComponent, error) {
	if len(selected) == 0 {
		return all, nil
	}
	selectedSet := map[string]struct{}{}
	for _, name := range selected {
		selectedSet[name] = struct{}{}
	}
	filtered := make([]common.ApplicationComponent, 0, len(selected))
	foundSet := map[string]struct{}{}
	for _, comp := range all {
		if _, ok := selectedSet[comp.Name]; ok {
			filtered = append(filtered, comp)
			foundSet[comp.Name] = struct{}{}
		}
	}
	missing := make([]string, 0)
	for _, name := range selected {
		if _, ok := foundSet[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, errors.Errorf("component(s) not found in application: %s", strings.Join(missing, ", "))
	}
	return filtered, nil
}

type dispatcherTemplateResult struct {
	ResolveTargets []v1alpha1.PlacementDecision      `json:"resolveTargets,omitempty"`
	Output         map[string]interface{}            `json:"output,omitempty"`
	Outputs        map[string]map[string]interface{} `json:"outputs,omitempty"`
}

type dispatcherTemplates struct {
	TargetsTemplate       string
	DispatchTemplate      string
	StatusMappingTemplate string
	HealthOverrideTemplate string
}

type dispatcherStatusMappingResult struct {
	Healthy *bool                             `json:"healthy,omitempty"`
	Message string                            `json:"message,omitempty"`
	Output  map[string]interface{}            `json:"output,omitempty"`
	Outputs map[string]map[string]interface{} `json:"outputs,omitempty"`
}

type dispatcherHealthOverrideResult struct {
	IsHealth *bool                  `json:"isHealth,omitempty"`
	Message  string                 `json:"message,omitempty"`
	Details  map[string]interface{} `json:"details,omitempty"`
}

func applyWithDispatcher(ctx context.Context, cli client.Client, kubeHandlers *wfprovidertypes.KubeHandlers, componentRender oamprovidertypes.ComponentRender, healthCheck oamprovidertypes.ComponentHealthCheck, dispatcherName string, af *appfile.Appfile, policies []v1beta1.AppPolicy, components []common.ApplicationComponent, placements []v1alpha1.PlacementDecision) (bool, string, error) {
	klog.Infof("dispatcher: start name=%s app=%s/%s inputPlacements=%d components=%d", dispatcherName, af.Namespace, af.Name, len(placements), len(components))
	dispatcherPolicies := buildDispatcherPoliciesForContext(policies)
	baseContext := buildDispatcherBaseContext(af, dispatcherName, dispatcherPolicies)
	templates, err := loadDispatcherTemplates(ctx, cli, af.Namespace, dispatcherName)
	if err != nil {
		return false, "", err
	}
	klog.Infof("dispatcher: loaded name=%s app=%s/%s", dispatcherName, af.Namespace, af.Name)
	resolvedPlacements, err := callDispatcherResolveTargets(ctx, templates.TargetsTemplate, nil, baseContext, placements)
	if err != nil {
		return false, "", err
	}
	if len(resolvedPlacements) == 0 {
		klog.Infof("dispatcher: resolveTargets empty; fallback to default placements name=%s app=%s/%s", dispatcherName, af.Namespace, af.Name)
		resolvedPlacements = placements
	} else {
		klog.Infof("dispatcher: resolved targets name=%s app=%s/%s count=%d", dispatcherName, af.Namespace, af.Name, len(resolvedPlacements))
	}
	for _, placement := range resolvedPlacements {
		klog.Infof("dispatcher: apply placement name=%s app=%s/%s cluster=%s namespace=%s", dispatcherName, af.Namespace, af.Name, placement.Cluster, placement.Namespace)
		for _, comp := range components {
			workload, traits, err := componentRender(ctx, comp, nil, placement.Cluster, placement.Namespace)
			if err != nil {
				return false, "", errors.Wrap(err, "render component and traits")
			}
			transformResult, err := callDispatcherTransform(ctx, templates.DispatchTemplate, nil, baseContext, placement, comp, workload, traits)
			if err != nil {
				return false, "", err
			}
			toApply := make([]map[string]interface{}, 0, 1+len(transformResult.Outputs))
			if len(transformResult.Output) > 0 {
				toApply = append(toApply, transformResult.Output)
			}
			for _, out := range transformResult.Outputs {
				toApply = append(toApply, out)
			}
			klog.Infof("dispatcher: prepared outputs name=%s app=%s/%s component=%s cluster=%s resources=%d", dispatcherName, af.Namespace, af.Name, comp.Name, placement.Cluster, len(toApply))
			for _, obj := range toApply {
				if err := dispatchUnstructured(ctx, cli, kubeHandlers, placement.Cluster, obj, af.Name, af.Namespace, comp.Name); err != nil {
					return false, "", err
				}
			}
			if healthCheck != nil {
				transformedResources := make([]*unstructured.Unstructured, 0, len(toApply))
				for _, obj := range toApply {
					transformedResources = append(transformedResources, &unstructured.Unstructured{Object: obj})
				}
				healthCtx := oamprovidertypes.WithDispatchHealthResources(ctx, transformedResources)
				var mappedHealth *oamprovidertypes.DispatchMappedHealth
				if templates.StatusMappingTemplate != "" {
					mapped, err := callDispatcherStatusMapping(ctx, cli, dispatcherName, templates.StatusMappingTemplate, baseContext, placement.Cluster, placement, comp, transformedResources)
					if err != nil {
						logDispatcherStatusMappingErrorDetails(dispatcherName, af.Namespace, af.Name, comp.Name, placement.Cluster, err)
					} else if mapped != nil {
						mappedHealth = &oamprovidertypes.DispatchMappedHealth{
							Healthy: mapped.Healthy,
							Message: mapped.Message,
							Output:  mapped.Output,
							Outputs: mapped.Outputs,
						}
					} else {
						klog.Infof("dispatcher: statusMapping returned nil name=%s app=%s/%s component=%s cluster=%s", dispatcherName, af.Namespace, af.Name, comp.Name, placement.Cluster)
					}
				} else {
					klog.Infof("dispatcher: statusMapping skipped name=%s app=%s/%s component=%s cluster=%s reason=no-template", dispatcherName, af.Namespace, af.Name, comp.Name, placement.Cluster)
				}
				if templates.HealthOverrideTemplate != "" {
					override, err := callDispatcherHealthOverride(ctx, cli, dispatcherName, templates.HealthOverrideTemplate, baseContext, placement.Cluster, placement, comp, transformedResources)
					if err != nil {
						reason := fmt.Sprintf("dispatcher %s health override pending for component %s in cluster %s: %v", dispatcherName, comp.Name, placement.Cluster, err)
						klog.Warningf("dispatcher: %s app=%s/%s", reason, af.Namespace, af.Name)
						return false, reason, nil
					}
					if override == nil || override.IsHealth == nil {
						reason := fmt.Sprintf("dispatcher %s health override pending for component %s in cluster %s: missing isHealth", dispatcherName, comp.Name, placement.Cluster)
						klog.Warningf("dispatcher: %s app=%s/%s", reason, af.Namespace, af.Name)
						return false, reason, nil
					}
					if !*override.IsHealth {
						klog.Infof("dispatcher: healthOverride unhealthy name=%s app=%s/%s component=%s cluster=%s reason=%s", dispatcherName, af.Namespace, af.Name, comp.Name, placement.Cluster, override.Message)
					}
					if mappedHealth == nil {
						mappedHealth = &oamprovidertypes.DispatchMappedHealth{}
					}
					mappedHealth.Healthy = override.IsHealth
					mappedHealth.Message = override.Message
					mappedHealth.Details = stringifyDetails(override.Details)
					klog.Infof("dispatcher: healthOverride evaluated name=%s app=%s/%s component=%s cluster=%s healthy=%t", dispatcherName, af.Namespace, af.Name, comp.Name, placement.Cluster, *override.IsHealth)
				}
				if mappedHealth != nil {
					healthCtx = oamprovidertypes.WithDispatchMappedHealth(healthCtx, mappedHealth)
				}
				if healthy, compStatus, _, _, err := healthCheck(healthCtx, comp, nil, placement.Cluster, placement.Namespace); err != nil {
					return false, "", fmt.Errorf("dispatcher %s health check failed for %s/%s component=%s cluster=%s: %w", dispatcherName, af.Namespace, af.Name, comp.Name, placement.Cluster, err)
				} else if !healthy {
					reason := ""
					if compStatus != nil && compStatus.Message != "" {
						reason = compStatus.Message
					}
					if reason == "" {
						reason = fmt.Sprintf("dispatcher %s component %s not healthy in cluster %s", dispatcherName, comp.Name, placement.Cluster)
					}
					klog.Infof("dispatcher: %s app=%s/%s", reason, af.Namespace, af.Name)
					return false, reason, nil
				}
			}
		}
	}
	return true, "", nil
}

func loadDispatcherTemplates(ctx context.Context, cli client.Client, appNamespace, name string) (*dispatcherTemplates, error) {
	namespaces := []string{appNamespace, types.DefaultKubeVelaNS}
	for _, ns := range namespaces {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion("core.oam.dev/v1beta1")
		u.SetKind("Dispatcher")
		if err := cli.Get(ctx, ktypes.NamespacedName{Name: name, Namespace: ns}, u); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return nil, err
			}
			continue
		}
		spec, ok := u.Object["spec"].(map[string]interface{})
		if !ok {
			return nil, errors.Errorf("dispatcher %s has invalid spec", name)
		}
		schematic, ok := spec["schematic"].(map[string]interface{})
		if !ok {
			return nil, errors.Errorf("dispatcher %s missing spec.schematic", name)
		}
		cueMap, ok := schematic["cue"].(map[string]interface{})
		if !ok {
			return nil, errors.Errorf("dispatcher %s missing spec.schematic.cue", name)
		}
		legacyTpl, _ := cueMap["template"].(string)
		targetsTpl, _ := cueMap["targetsTemplate"].(string)
		dispatchTpl, _ := cueMap["dispatchTemplate"].(string)
		statusMappingTpl, _ := cueMap["statusMappingTemplate"].(string)
		healthOverrideTpl, _ := cueMap["healthOverrideTemplate"].(string)
		if targetsTpl == "" {
			targetsTpl = legacyTpl
		}
		if dispatchTpl == "" {
			dispatchTpl = legacyTpl
		}
		if targetsTpl == "" && dispatchTpl == "" {
			return nil, errors.Errorf("dispatcher %s missing spec.schematic.cue.(template|targetsTemplate|dispatchTemplate)", name)
		}
		return &dispatcherTemplates{
			TargetsTemplate:       targetsTpl,
			DispatchTemplate:      dispatchTpl,
			StatusMappingTemplate: statusMappingTpl,
			HealthOverrideTemplate: healthOverrideTpl,
		}, nil
	}
	return nil, errors.Errorf("dispatcher %s not found", name)
}

func buildDispatcherPoliciesForContext(policies []v1beta1.AppPolicy) []map[string]interface{} {
	res := make([]map[string]interface{}, 0, len(policies))
	for _, p := range policies {
		pm := map[string]interface{}{
			"name": p.Name,
			"type": p.Type,
		}
		if p.Properties != nil && len(p.Properties.Raw) > 0 {
			var props map[string]interface{}
			if err := json.Unmarshal(p.Properties.Raw, &props); err != nil {
				klog.Warningf("dispatcher: failed to decode policy properties name=%s type=%s err=%v", p.Name, p.Type, err)
			} else {
				pm["properties"] = props
			}
		}
		res = append(res, pm)
	}
	return res
}

// ResolveDispatcherStatusMapping evaluates dispatcher statusMappingTemplate against latest resources.
func ResolveDispatcherStatusMapping(ctx context.Context, cli client.Client, af *appfile.Appfile, dispatcherName string, cluster string, placement v1alpha1.PlacementDecision, component common.ApplicationComponent, policies []v1beta1.AppPolicy, resources []*unstructured.Unstructured) (*oamprovidertypes.DispatchMappedHealth, error) {
	templates, err := loadDispatcherTemplates(ctx, cli, af.Namespace, dispatcherName)
	if err != nil {
		return nil, err
	}
	if templates.StatusMappingTemplate == "" {
		return nil, nil
	}
	dispatcherPolicies := buildDispatcherPoliciesForContext(policies)
	baseContext := buildDispatcherBaseContext(af, dispatcherName, dispatcherPolicies)
	mapped, err := callDispatcherStatusMapping(ctx, cli, dispatcherName, templates.StatusMappingTemplate, baseContext, cluster, placement, component, resources)
	if err != nil {
		return nil, err
	}
	if mapped == nil {
		return nil, nil
	}
	return &oamprovidertypes.DispatchMappedHealth{
		Healthy: mapped.Healthy,
		Message: mapped.Message,
		Output:  mapped.Output,
		Outputs: mapped.Outputs,
	}, nil
}

func callDispatcherResolveTargets(ctx context.Context, template string, params map[string]interface{}, baseContext map[string]interface{}, placements []v1alpha1.PlacementDecision) ([]v1alpha1.PlacementDecision, error) {
	val, err := compileDispatcherTemplate(ctx, template, params, mergeDispatcherContext(baseContext, map[string]interface{}{
		"placements": placements,
	}))
	if err != nil {
		return nil, err
	}
	resolveVal := val.LookupPath(cue.ParsePath("targets"))
	if !resolveVal.Exists() {
		// Backward compatibility with older dispatcher templates.
		resolveVal = val.LookupPath(cue.ParsePath("resolveTargets"))
	}
	if !resolveVal.Exists() {
		return nil, nil
	}
	bs, err := resolveVal.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var targets []v1alpha1.PlacementDecision
	if err := json.Unmarshal(bs, &targets); err != nil {
		return nil, err
	}
	return targets, nil
}

func callDispatcherTransform(ctx context.Context, template string, params map[string]interface{}, baseContext map[string]interface{}, placement v1alpha1.PlacementDecision, component common.ApplicationComponent, workload *unstructured.Unstructured, traits []*unstructured.Unstructured) (*dispatcherTemplateResult, error) {
	traitMap := map[string]map[string]interface{}{}
	for i, tr := range traits {
		if tr == nil {
			continue
		}
		name := tr.GetLabels()[oam.TraitResource]
		if name == "" {
			name = fmt.Sprintf("trait-%d", i)
		}
		traitMap[name] = tr.Object
	}
	workloadObj := map[string]interface{}{}
	if workload != nil {
		workloadObj = workload.Object
	}
	val, err := compileDispatcherTemplate(ctx, template, params, mergeDispatcherContext(baseContext, map[string]interface{}{
		"placement": placement,
		"component": component,
		"output":    workloadObj,
		"outputs":   traitMap,
	}))
	if err != nil {
		return nil, err
	}
	result := &dispatcherTemplateResult{}
	outputVal := val.LookupPath(cue.ParsePath("output"))
	if outputVal.Exists() {
		bs, err := outputVal.MarshalJSON()
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(bs, &result.Output); err != nil {
			return nil, err
		}
	}
	outputsVal := val.LookupPath(cue.ParsePath("outputs"))
	if outputsVal.Exists() {
		bs, err := outputsVal.MarshalJSON()
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(bs, &result.Outputs); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func logDispatcherStatusMappingErrorDetails(dispatcherName, appNamespace, appName, componentName, cluster string, err error) {
	klog.Warningf("dispatcher: statusMapping failed name=%s app=%s/%s component=%s cluster=%s err=%v", dispatcherName, appNamespace, appName, componentName, cluster, err)
	errs := cueerrors.Errors(err)
	if len(errs) <= 1 {
		return
	}
	for i, e := range errs {
		klog.Warningf(
			"dispatcher: statusMapping failed detail name=%s app=%s/%s component=%s cluster=%s index=%d/%d err=%s",
			dispatcherName, appNamespace, appName, componentName, cluster, i+1, len(errs), e.Error(),
		)
	}
}

func callDispatcherStatusMapping(ctx context.Context, cli client.Client, dispatcherName string, template string, baseContext map[string]interface{}, cluster string, placement v1alpha1.PlacementDecision, component common.ApplicationComponent, resources []*unstructured.Unstructured) (*dispatcherStatusMappingResult, error) {
	if template == "" {
		return nil, nil
	}
	outputObj := map[string]interface{}{}
	outputsObj := map[string]map[string]interface{}{}
	for i, res := range resources {
		if res == nil {
			continue
		}
		latest := res.DeepCopy()
		targetCtx := multicluster.ContextWithClusterName(ctx, cluster)
		key := ktypes.NamespacedName{Name: latest.GetName(), Namespace: latest.GetNamespace()}
		if err := cli.Get(targetCtx, key, latest); err == nil {
			// use fetched latest status/object
		}
		if i == 0 {
			outputObj = latest.Object
		} else {
			key := latest.GetKind()
			if latest.GetName() != "" {
				key = latest.GetName()
			}
			outputsObj[key] = latest.Object
		}
	}
	val, err := compileDispatcherTemplate(ctx, template, nil, mergeDispatcherContext(baseContext, map[string]interface{}{
		"placement": placement,
		"component": component,
		"output":    outputObj,
		"outputs":   outputsObj,
		"resources": map[string]interface{}{
			"output":  outputObj,
			"outputs": outputsObj,
		},
	}))
	if err != nil {
		return nil, err
	}
	bs, err := val.MarshalJSON()
	if err != nil {
		return nil, err
	}
	result := &dispatcherStatusMappingResult{}
	if err := json.Unmarshal(bs, result); err != nil {
		return nil, err
	}
	normalizeMappedStatusKeys(result)
	klog.V(1).Infof("dispatcher: statusMapping evaluated name=%s component=%s cluster=%s", dispatcherName, component.Name, cluster)
	return result, nil
}

func callDispatcherHealthOverride(ctx context.Context, cli client.Client, dispatcherName string, template string, baseContext map[string]interface{}, cluster string, placement v1alpha1.PlacementDecision, component common.ApplicationComponent, resources []*unstructured.Unstructured) (*dispatcherHealthOverrideResult, error) {
	if template == "" {
		return nil, nil
	}
	outputObj := map[string]interface{}{}
	outputsObj := map[string]map[string]interface{}{}
	for i, res := range resources {
		if res == nil {
			continue
		}
		latest := res.DeepCopy()
		targetCtx := multicluster.ContextWithClusterName(ctx, cluster)
		key := ktypes.NamespacedName{Name: latest.GetName(), Namespace: latest.GetNamespace()}
		if err := cli.Get(targetCtx, key, latest); err == nil {
			// use fetched latest status/object
		}
		if i == 0 {
			outputObj = latest.Object
		} else {
			key := latest.GetKind()
			if latest.GetName() != "" {
				key = latest.GetName()
			}
			outputsObj[key] = latest.Object
		}
	}
	runtimeCtx := mergeDispatcherContext(baseContext, map[string]interface{}{
		"placement": placement,
		"component": component,
		"output":    outputObj,
		"outputs":   outputsObj,
		"resources": map[string]interface{}{
			"output":  outputObj,
			"outputs": outputsObj,
		},
	})
	if klog.V(1).Enabled() {
		klog.Infof(
			"dispatcher: healthOverride input name=%s component=%s cluster=%s context=\n%s",
			dispatcherName, component.Name, cluster, prettyJSON(runtimeCtx),
		)
	}
	val, err := compileDispatcherTemplate(ctx, template, nil, runtimeCtx)
	if err != nil {
		return nil, err
	}
	bs, err := val.MarshalJSON()
	if err != nil {
		return nil, err
	}
	result := &dispatcherHealthOverrideResult{}
	if err := json.Unmarshal(bs, result); err != nil {
		return nil, err
	}
	if klog.V(1).Enabled() {
		klog.Infof(
			"dispatcher: healthOverride output name=%s component=%s cluster=%s output=\n%s",
			dispatcherName, component.Name, cluster, prettyJSON(result),
		)
	}
	klog.V(1).Infof("dispatcher: healthOverride evaluated name=%s component=%s cluster=%s", dispatcherName, component.Name, cluster)
	return result, nil
}

func stringifyDetails(details map[string]interface{}) map[string]string {
	if len(details) == 0 {
		return nil
	}
	result := make(map[string]string, len(details))
	for key, value := range details {
		switch v := value.(type) {
		case string:
			result[key] = v
		default:
			result[key] = fmt.Sprintf("%v", v)
		}
	}
	return result
}

func buildDispatcherBaseContext(af *appfile.Appfile, dispatcherName string, policies []map[string]interface{}) map[string]interface{} {
	ctx := map[string]interface{}{
		"dispatcher": dispatcherName,
		"policies":   policies,
	}
	if af == nil {
		return ctx
	}
	ctx["appName"] = af.Name
	ctx["namespace"] = af.Namespace
	ctx["appRevision"] = af.AppRevisionName
	ctx["workflowName"] = af.AppAnnotations[oam.AnnotationWorkflowName]
	ctx["publishVersion"] = af.AppAnnotations[oam.AnnotationPublishVersion]
	ctx["appLabels"] = af.AppLabels
	ctx["appAnnotations"] = af.AppAnnotations
	return ctx
}

func mergeDispatcherContext(base map[string]interface{}, extra map[string]interface{}) map[string]interface{} {
	merged := make(map[string]interface{}, len(base)+len(extra))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range extra {
		merged[key] = value
	}
	return merged
}

func prettyJSON(v interface{}) string {
	bs, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(bs)
}

func normalizeMappedStatusKeys(result *dispatcherStatusMappingResult) {
	if result == nil {
		return
	}
	if result.Output != nil {
		if status, ok := result.Output["status"].(map[string]interface{}); ok {
			normalizeStatusKeysToLowerCamel(status)
		}
	}
	for _, out := range result.Outputs {
		if out == nil {
			continue
		}
		if status, ok := out["status"].(map[string]interface{}); ok {
			normalizeStatusKeysToLowerCamel(status)
		}
	}
}

func normalizeStatusKeysToLowerCamel(status map[string]interface{}) {
	if status == nil {
		return
	}
	type kv struct {
		from string
		to   string
	}
	renames := make([]kv, 0)
	for key := range status {
		normalized := lowerFirstRune(key)
		if normalized == key {
			continue
		}
		if _, exists := status[normalized]; exists {
			continue
		}
		renames = append(renames, kv{from: key, to: normalized})
	}
	for _, rename := range renames {
		status[rename.to] = status[rename.from]
		delete(status, rename.from)
	}
}

func lowerFirstRune(s string) string {
	if s == "" {
		return s
	}
	rs := []rune(s)
	rs[0] = unicode.ToLower(rs[0])
	return string(rs)
}

func compileDispatcherTemplate(ctx context.Context, template string, params map[string]interface{}, runtimeContext map[string]interface{}) (cue.Value, error) {
	if params == nil {
		params = map[string]interface{}{}
	}
	v, err := dispatchruntime.Compiler.Get().CompileStringWithOptions(
		ctx,
		template,
		cuex.WithExtraData("context", runtimeContext),
		cuex.WithExtraData("template.parameter", params),
	)
	if err != nil {
		return cue.Value{}, err
	}
	return v, nil
}

func dispatchUnstructured(ctx context.Context, cli client.Client, kubeHandlers *wfprovidertypes.KubeHandlers, cluster string, obj map[string]interface{}, appName string, appNamespace string, componentName string) error {
	u := &unstructured.Unstructured{Object: obj}
	labels := u.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	if appName != "" {
		labels[oam.LabelAppName] = appName
	}
	if appNamespace != "" {
		labels[oam.LabelAppNamespace] = appNamespace
	}
	if componentName != "" {
		labels[oam.LabelAppComponent] = componentName
	}
	if cluster != "" {
		labels[oam.LabelAppCluster] = cluster
	}
	u.SetLabels(labels)
	name := u.GetName()
	namespace := u.GetNamespace()
	if name == "" {
		return errors.New("dispatcher transform output object missing metadata.name")
	}
	if kubeHandlers != nil && kubeHandlers.Apply != nil {
		return kubeHandlers.Apply(ctx, cli, cluster, common.WorkflowResourceCreator, u)
	}
	targetCtx := multicluster.ContextWithClusterName(ctx, cluster)
	existing := &unstructured.Unstructured{}
	existing.SetAPIVersion(u.GetAPIVersion())
	existing.SetKind(u.GetKind())
	key := ktypes.NamespacedName{Name: name, Namespace: namespace}
	if err := cli.Get(targetCtx, key, existing); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		return cli.Create(targetCtx, u)
	}
	existing.Object = u.Object
	return cli.Update(targetCtx, existing)
}

func selectPolicies(policies []v1beta1.AppPolicy, policyNames []string) ([]v1beta1.AppPolicy, error) {
	policyMap := make(map[string]v1beta1.AppPolicy)
	for _, policy := range policies {
		policyMap[policy.Name] = policy
	}
	var selectedPolicies []v1beta1.AppPolicy
	for _, policyName := range policyNames {
		if policy, found := policyMap[policyName]; found {
			selectedPolicies = append(selectedPolicies, policy)
		} else {
			return nil, errors.Errorf("policy %s not found", policyName)
		}
	}
	return selectedPolicies, nil
}

func fillInlinePolicyNames(policies []v1beta1.AppPolicy) []v1beta1.AppPolicy {
	for i := range policies {
		if policies[i].Name == "" {
			policies[i].Name = fmt.Sprintf("inline-%s-policy-%d", policies[i].Type, i)
		}
	}
	return policies
}

func loadComponents(ctx context.Context, render oamprovidertypes.WorkloadRender, cli client.Client, af *appfile.Appfile, components []common.ApplicationComponent, ignoreTerraformComponent bool) ([]common.ApplicationComponent, error) {
	var loadedComponents []common.ApplicationComponent
	for _, comp := range components {
		loadedComp, err := af.LoadDynamicComponent(ctx, cli, comp.DeepCopy())
		if err != nil {
			return nil, err
		}
		if ignoreTerraformComponent {
			wl, err := render(ctx, comp)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to render component into workload")
			}
			if wl.CapabilityCategory == types.TerraformCategory {
				continue
			}
		}
		loadedComponents = append(loadedComponents, *loadedComp)
	}
	return loadedComponents, nil
}

func overrideConfiguration(policies []v1beta1.AppPolicy, components []common.ApplicationComponent) ([]common.ApplicationComponent, error) {
	var err error
	for _, policy := range policies {
		if policy.Type == v1alpha1.OverridePolicyType {
			if policy.Properties == nil {
				return nil, fmt.Errorf("override policy %s must not have empty properties", policy.Name)
			}
			overrideSpec := &v1alpha1.OverridePolicySpec{}
			if err := utils.StrictUnmarshal(policy.Properties.Raw, overrideSpec); err != nil {
				return nil, errors.Wrapf(err, "failed to parse override policy %s", policy.Name)
			}
			components, err = envbinding.PatchComponents(components, overrideSpec.Components, overrideSpec.Selector)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to apply override policy %s", policy.Name)
			}
		}
	}
	return components, nil
}

type valueBuilder func(s string) cue.Value

type applyTask struct {
	component common.ApplicationComponent
	placement v1alpha1.PlacementDecision
	healthy   *bool
}

func (t *applyTask) key() string {
	return fmt.Sprintf("%s/%s/%s/%s", t.placement.Cluster, t.placement.Namespace, t.component.ReplicaKey, t.component.Name)
}

func (t *applyTask) varKey(v string) string {
	return fmt.Sprintf("%s/%s/%s/%s", t.placement.Cluster, t.placement.Namespace, t.component.ReplicaKey, v)
}

func (t *applyTask) varKeyWithoutReplica(v string) string {
	return fmt.Sprintf("%s/%s/%s/%s", t.placement.Cluster, t.placement.Namespace, "", v)
}

func (t *applyTask) getVar(from string, cache *pkgmaps.SyncMap[string, cue.Value]) cue.Value {
	key := t.varKey(from)
	keyWithNoReplica := t.varKeyWithoutReplica(from)
	var val cue.Value
	var ok bool
	if val, ok = cache.Get(key); !ok {
		if val, ok = cache.Get(keyWithNoReplica); !ok {
			return cue.Value{}
		}
	}
	return val
}

func (t *applyTask) fillInputs(inputs *pkgmaps.SyncMap[string, cue.Value], build valueBuilder) error {
	if len(t.component.Inputs) == 0 {
		return nil
	}
	var err error
	x := component2Value(t.component, build)
	for _, input := range t.component.Inputs {
		var inputVal cue.Value
		if inputVal = t.getVar(input.From, inputs); inputVal == (cue.Value{}) {
			return fmt.Errorf("input %s is not ready", input)
		}

		x, err = value.SetValueByScript(x, inputVal, fieldPathToComponent(input.ParameterKey))
		if err != nil {
			return errors.Wrap(err, "fill value to component")
		}
	}
	newComp, err := value2Component(x)
	if err != nil {
		return err
	}
	t.component = *newComp
	return nil
}

func (t *applyTask) generateOutput(output *unstructured.Unstructured, outputs []*unstructured.Unstructured, cache *pkgmaps.SyncMap[string, cue.Value], build valueBuilder) error {
	if len(t.component.Outputs) == 0 {
		return nil
	}

	var cueString string
	if output != nil {
		outputJSON, err := output.MarshalJSON()
		if err != nil {
			return errors.Wrap(err, "marshal output")
		}
		cueString += fmt.Sprintf("output:%s\n", string(outputJSON))
	}
	componentVal := build(cueString)

	for _, os := range outputs {
		name := os.GetLabels()[oam.TraitResource]
		if name != "" {
			componentVal = componentVal.FillPath(cue.ParsePath(fmt.Sprintf("outputs.%s", name)), os.Object)
		}
	}

	for _, o := range t.component.Outputs {
		pathToSetVar := t.varKey(o.Name)
		actualOutput := componentVal.LookupPath(cue.ParsePath(o.ValueFrom))
		if !actualOutput.Exists() {
			return workflowerrors.LookUpNotFoundErr(o.ValueFrom)
		}
		cache.Set(pathToSetVar, actualOutput)
	}
	return nil
}

func (t *applyTask) allDependsReady(healthyMap map[string]bool) bool {
	for _, d := range t.component.DependsOn {
		dKey := fmt.Sprintf("%s/%s/%s/%s", t.placement.Cluster, t.placement.Namespace, t.component.ReplicaKey, d)
		dKeyWithoutReplica := fmt.Sprintf("%s/%s/%s/%s", t.placement.Cluster, t.placement.Namespace, "", d)
		if !healthyMap[dKey] && !healthyMap[dKeyWithoutReplica] {
			return false
		}
	}
	return true
}

func (t *applyTask) allInputReady(cache *pkgmaps.SyncMap[string, cue.Value]) bool {
	for _, in := range t.component.Inputs {
		if val := t.getVar(in.From, cache); val == (cue.Value{}) {
			return false
		}
	}

	return true
}

type applyTaskResult struct {
	healthy bool
	err     error
	task    *applyTask
	// outputReady indicates whether all declared outputs are ready
	outputReady bool
}

// applyComponents will apply components to placements.
// nolint:gocyclo
func applyComponents(ctx context.Context, apply oamprovidertypes.ComponentApply, healthCheck oamprovidertypes.ComponentHealthCheck, components []common.ApplicationComponent, placements []v1alpha1.PlacementDecision, parallelism int) (bool, string, error) {
	var tasks []*applyTask
	var cache = pkgmaps.NewSyncMap[string, cue.Value]()
	rootValue := cuecontext.New().CompileString("{}")
	if rootValue.Err() != nil {
		return false, "", rootValue.Err()
	}
	var cueMutex sync.Mutex
	var makeValue = func(s string) cue.Value {
		cueMutex.Lock()
		defer cueMutex.Unlock()
		return rootValue.Context().CompileString(s)
	}

	taskHealthyMap := map[string]bool{}
	for _, comp := range components {
		for _, pl := range placements {
			tasks = append(tasks, &applyTask{component: comp, placement: pl})
		}
	}
	unhealthyResults := make([]*applyTaskResult, 0)
	maxHealthCheckTimes := len(tasks)
	outputNotReadyReasons := make([]string, 0)
	outputsReady := true
HealthCheck:
	for i := 0; i < maxHealthCheckTimes; i++ {
		checkTasks := make([]*applyTask, 0)
		for _, task := range tasks {
			if task.healthy == nil && task.allDependsReady(taskHealthyMap) && task.allInputReady(cache) {
				task.healthy = new(bool)
				err := task.fillInputs(cache, makeValue)
				if err != nil {
					taskHealthyMap[task.key()] = false
					unhealthyResults = append(unhealthyResults, &applyTaskResult{healthy: false, err: err, task: task})
					continue
				}
				checkTasks = append(checkTasks, task)
			}
		}
		if len(checkTasks) == 0 {
			break HealthCheck
		}
		checkResults := slices.ParMap[*applyTask, *applyTaskResult](checkTasks, func(task *applyTask) *applyTaskResult {
			healthy, _, output, outputs, err := healthCheck(ctx, task.component, nil, task.placement.Cluster, task.placement.Namespace)
			task.healthy = ptr.To(healthy)
			if healthy {
				if errOutput := task.generateOutput(output, outputs, cache, makeValue); errOutput != nil {
					var notFound workflowerrors.LookUpNotFoundErr
					if errors.As(errOutput, &notFound) && strings.HasPrefix(string(notFound), "outputs.") && len(outputs) == 0 {
						// PostDispatch traits are not rendered/applied yet, so trait outputs are unavailable.
						// Skip blocking the deploy step; the outputs will be populated after PostDispatch runs.
						errOutput = nil
					}
					err = errOutput
				}
			}
			return &applyTaskResult{healthy: healthy, err: err, task: task, outputReady: true}
		}, slices.Parallelism(parallelism))

		for _, res := range checkResults {
			taskHealthyMap[res.task.key()] = res.healthy
			if !res.outputReady {
				outputsReady = false
				outputNotReadyReasons = append(outputNotReadyReasons, fmt.Sprintf("%s outputs not ready", res.task.key()))
			}
			if !res.healthy || res.err != nil {
				unhealthyResults = append(unhealthyResults, res)
			}
		}
	}

	var pendingTasks []*applyTask
	var todoTasks []*applyTask

	for _, task := range tasks {
		if healthy, ok := taskHealthyMap[task.key()]; healthy && ok {
			continue
		}
		if task.allDependsReady(taskHealthyMap) && task.allInputReady(cache) {
			todoTasks = append(todoTasks, task)
		} else {
			pendingTasks = append(pendingTasks, task)
		}
	}
	var results []*applyTaskResult
	if len(todoTasks) > 0 {
		results = slices.ParMap[*applyTask, *applyTaskResult](todoTasks, func(task *applyTask) *applyTaskResult {
			err := task.fillInputs(cache, makeValue)
			if err != nil {
				return &applyTaskResult{healthy: false, err: err, task: task, outputReady: true}
			}
			_, _, healthy, err := apply(ctx, task.component, nil, task.placement.Cluster, task.placement.Namespace)
			if err != nil {
				return &applyTaskResult{healthy: healthy, err: err, task: task, outputReady: true}
			}
			return &applyTaskResult{healthy: healthy, err: err, task: task, outputReady: true}
		}, slices.Parallelism(parallelism))
	}
	var errs []error
	var allHealthy = true
	var reasons []string
	for _, res := range unhealthyResults {
		if res.err != nil {
			errs = append(errs, fmt.Errorf("error health check from %s: %w", res.task.key(), res.err))
		}
	}
	for _, res := range results {
		if res.err != nil {
			errs = append(errs, fmt.Errorf("error encountered in cluster %s: %w", res.task.placement.Cluster, res.err))
		}
		if !res.healthy {
			allHealthy = false
			reasons = append(reasons, fmt.Sprintf("%s is not healthy", res.task.key()))
		}
	}

	reasons = append(reasons, outputNotReadyReasons...)

	for _, t := range pendingTasks {
		reasons = append(reasons, fmt.Sprintf("%s is waiting dependents", t.key()))
	}

	return allHealthy && outputsReady && len(pendingTasks) == 0, strings.Join(reasons, ","), velaerrors.AggregateErrors(errs)
}

func fieldPathToComponent(input string) string {
	return fmt.Sprintf("properties.%s", strings.TrimSpace(input))
}

func component2Value(comp common.ApplicationComponent, build valueBuilder) cue.Value {
	x := build("")
	x = x.FillPath(cue.ParsePath(""), comp)
	// Component.ReplicaKey have no json tag, so we need to set it manually
	x = x.FillPath(cue.ParsePath("replicaKey"), comp.ReplicaKey)
	return x
}

func value2Component(v cue.Value) (*common.ApplicationComponent, error) {
	var comp common.ApplicationComponent
	err := value.UnmarshalTo(v, &comp)
	if err != nil {
		return nil, err
	}
	if rk, err := v.LookupPath(cue.ParsePath("replicaKey")).String(); err == nil {
		comp.ReplicaKey = rk
	}
	return &comp, nil
}
