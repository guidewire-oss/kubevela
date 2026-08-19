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

package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	pkgmulticluster "github.com/kubevela/pkg/multicluster"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/pkg/cue/definition"
	"github.com/oam-dev/kubevela/pkg/oam"
	oamutil "github.com/oam-dev/kubevela/pkg/oam/util"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/appfile"
)

// DispatchOptions is the options for dispatch
type DispatchOptions struct {
	Workload          *unstructured.Unstructured
	Traits            []*unstructured.Unstructured
	OverrideNamespace string
	Stage             StageType
}

// SortDispatchOptions describe the sorting for options
type SortDispatchOptions []DispatchOptions

func (s SortDispatchOptions) Len() int {
	return len(s)
}

func (s SortDispatchOptions) Less(i, j int) bool {
	return s[i].Stage < s[j].Stage
}

func (s SortDispatchOptions) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

var _ sort.Interface = new(SortDispatchOptions)

// StageType is a valid value for TraitDefinitionSpec.Stage
type StageType int

const (
	// PreDispatch means that pre dispatch for manifests
	PreDispatch StageType = iota
	// DefaultDispatch means that default dispatch for manifests
	DefaultDispatch
	// PostDispatch means that post dispatch for manifests
	PostDispatch
)

var stages = map[StageType]string{
	PreDispatch:     "PreDispatch",
	DefaultDispatch: "DefaultDispatch",
	PostDispatch:    "PostDispatch",
}

// ParseStageType parse the StageType from a string
func ParseStageType(s string) (StageType, error) {
	for k, v := range stages {
		if v == s {
			return k, nil
		}
	}
	return -1, errors.New("unknown stage type")
}

// TraitFilter is used to filter trait object.
type TraitFilter func(trait appfile.Trait) bool

// ByTraitType returns a filter that does not match the given type and belongs to readyTraits.
func ByTraitType(readyTraits, checkTraits []*unstructured.Unstructured) TraitFilter {
	generateFn := func(manifests []*unstructured.Unstructured) map[string]bool {
		out := map[string]bool{}
		for _, obj := range manifests {
			out[obj.GetLabels()[oam.TraitTypeLabel]] = true
		}
		return out
	}
	readyMap := generateFn(readyTraits)
	checkMap := generateFn(checkTraits)
	return func(trait appfile.Trait) bool {
		return !checkMap[trait.Name] && readyMap[trait.Name]
	}
}

// manifestDispatcher is a manifest dispatcher
type manifestDispatcher struct {
	run         func(ctx context.Context, c *appfile.Component, appRev *v1beta1.ApplicationRevision, clusterName string) (bool, error)
	healthCheck func(ctx context.Context, c *appfile.Component, appRev *v1beta1.ApplicationRevision) (bool, error)
}

func (h *AppHandler) generateDispatcher(appRev *v1beta1.ApplicationRevision, previousAppRev *v1beta1.ApplicationRevision, readyWorkload *unstructured.Unstructured, readyTraits []*unstructured.Unstructured, overrideNamespace string, annotations map[string]string) ([]*manifestDispatcher, error) {
	dispatcherGenerator := func(options DispatchOptions) *manifestDispatcher {
		assembleManifestFn := func(skipApplyWorkload bool) (bool, []*unstructured.Unstructured) {
			manifests := options.Traits
			skipWorkload := skipApplyWorkload || options.Workload == nil
			if !skipWorkload {
				manifests = append([]*unstructured.Unstructured{options.Workload}, options.Traits...)
			}
			return skipWorkload, manifests
		}

		dispatcher := new(manifestDispatcher)
		dispatcher.healthCheck = func(ctx context.Context, comp *appfile.Component, appRev *v1beta1.ApplicationRevision) (bool, error) {
			skipWorkload, manifests := assembleManifestFn(comp.SkipApplyWorkload)
			if !h.resourceKeeper.ContainsResources(manifests) {
				return false, nil
			}
			_, _, _, isHealth, err := h.collectHealthStatus(ctx, comp, options.OverrideNamespace, skipWorkload,
				ByTraitType(readyTraits, options.Traits))
			if err != nil {
				return false, err
			}
			return isHealth, nil
		}
		dispatcher.run = func(ctx context.Context, comp *appfile.Component, appRev *v1beta1.ApplicationRevision, clusterName string) (bool, error) {
			skipWorkload, dispatchManifests := assembleManifestFn(comp.SkipApplyWorkload)

			var isAutoUpdateEnabled bool
			if annotations[oam.AnnotationAutoUpdate] == "true" {
				isAutoUpdateEnabled = true
			}

			isHealth, err := dispatcher.healthCheck(ctx, comp, appRev)

			// Check if component properties have changed (only for healthy components)
			// Note: componentPropertiesChanged handles nil comp.Params correctly, so we don't check it here
			propertiesChanged := false
			if isHealth && err == nil {
				comparisonRev := appRev // Default: use currentAppRev (existing behavior)

				// If autoRevision=true and we have a previous revision, compare against it
				// This detects policy-driven changes when ApplicationRevisions are created
				if annotations[oam.AnnotationAutoRevision] == "true" && previousAppRev != nil {
					comparisonRev = previousAppRev // Use previous revision for comparison
				}

				propertiesChanged = componentPropertiesChanged(comp, comparisonRev)
			}

			// source values are resolved at render time and are invisible to
			// the raw spec comparison above (comp.Params still holds the
			// unresolved directive). When opted in via autoUpdate /
			// autoUpdateSources, detect a re-resolved value by comparing
			// per-source hashes against those stamped on the live workload, and
			// re-dispatch when a selected source changed.
			resolvedHashes, consumesSource := resolvedSourceHashes(comp)
			matchAll, selected, sourceUpdateState := sourceAutoUpdateSelector(annotations)
			sourceUpdateEnabled := sourceUpdateState.enabled(sourceAutoUpdateDefault())
			sourceValuesChanged := false
			if isHealth && err == nil && consumesSource && sourceUpdateEnabled && !skipWorkload && options.Workload != nil {
				live := liveResolvedSourceHashes(ctx, h.Client, clusterName, options.Workload)
				for _, name := range changedSources(resolvedHashes, live) {
					if matchAll {
						sourceValuesChanged = true
						break
					}
					if _, ok := selected[name]; ok {
						sourceValuesChanged = true
						break
					}
				}
			}

			// Dispatch if: unhealthy, health error, properties changed, source
			// values changed, or auto-update enabled
			requiresDispatch := !isHealth || err != nil || propertiesChanged || sourceValuesChanged || (!comp.SkipApplyWorkload && isAutoUpdateEnabled)

			if requiresDispatch {
				// Record the resolved-source hashes so the next reconcile can
				// detect a subsequent change. Stamp whenever the component
				// consumes sources, so the baseline exists even before opt-in.
				if consumesSource {
					stampResolvedSourceHashes(options.Workload, resolvedHashes)
				}
				if err := h.Dispatch(ctx, h.Client, clusterName, common.WorkflowResourceCreator, dispatchManifests...); err != nil {
					return false, errors.WithMessage(err, "Dispatch")
				}
				status, _, _, isHealth, err := h.collectHealthStatus(ctx, comp, options.OverrideNamespace, skipWorkload,
					ByTraitType(readyTraits, options.Traits))
				if err != nil {
					return false, errors.WithMessage(err, "CollectHealthStatus")
				}
				if options.Stage < DefaultDispatch {
					status.Healthy = false
					if status.Message == "" {
						status.Message = "waiting for previous stage healthy"
					}
					h.addServiceStatus(true, *status)
				}
				if !isHealth {
					return false, nil
				}
			}
			return true, nil
		}
		return dispatcher
	}

	traitStageMap := make(map[StageType][]*unstructured.Unstructured)
	for _, readyTrait := range readyTraits {
		var (
			traitType = readyTrait.GetLabels()[oam.TraitTypeLabel]
			stageType = DefaultDispatch
			err       error
		)
		switch {
		case traitType == definition.AuxiliaryWorkload:
		case traitType != "":
			if strings.Contains(traitType, "-") {
				splitName := traitType[0:strings.LastIndex(traitType, "-")]
				if _, ok := appRev.Spec.TraitDefinitions[splitName]; ok {
					traitType = splitName
				}
			}
			stageType, err = getTraitDispatchStage(h.Client, traitType, appRev, annotations)
			if err != nil {
				return nil, err
			}
		}
		traitStageMap[stageType] = append(traitStageMap[stageType], readyTrait)
	}
	var optionList SortDispatchOptions
	if _, ok := traitStageMap[DefaultDispatch]; !ok {
		traitStageMap[DefaultDispatch] = []*unstructured.Unstructured{}
	}
	for stage, traits := range traitStageMap {
		option := DispatchOptions{
			Stage:             stage,
			Traits:            traits,
			OverrideNamespace: overrideNamespace,
		}
		if stage == DefaultDispatch {
			option.Workload = readyWorkload
		}
		optionList = append(optionList, option)
	}
	sort.Sort(optionList)

	var manifestDispatchers []*manifestDispatcher
	for _, option := range optionList {
		manifestDispatchers = append(manifestDispatchers, dispatcherGenerator(option))
	}
	return manifestDispatchers, nil
}

func getTraitDispatchStage(client client.Client, traitType string, appRev *v1beta1.ApplicationRevision, annotations map[string]string) (StageType, error) {
	trait, ok := appRev.Spec.TraitDefinitions[traitType]
	if !ok {
		trait = &v1beta1.TraitDefinition{}
		err := oamutil.GetCapabilityDefinition(context.Background(), client, trait, traitType, annotations)
		if err != nil {
			return DefaultDispatch, err
		}
	}
	_stageType := trait.Spec.Stage
	if len(_stageType) == 0 {
		_stageType = v1beta1.DefaultDispatch
	}
	stageType, err := ParseStageType(string(_stageType))
	if err != nil {
		return DefaultDispatch, err
	}
	return stageType, nil
}

// componentPropertiesChanged compares current component properties against an ApplicationRevision.
//
// When autoRevision=true (policy transforms create revisions):
//   - Compares against the PREVIOUS revision to detect policy-driven changes
//   - This ensures policies that transform components trigger redeployment
//
// When autoRevision=false or not set (default):
//   - Compares against the CURRENT revision
//   - This detects when workflow steps dynamically modify component properties
//
// Returns true if properties have changed.
func componentPropertiesChanged(comp *appfile.Component, appRev *v1beta1.ApplicationRevision) bool {
	var revComponent *common.ApplicationComponent
	for i := range appRev.Spec.Application.Spec.Components {
		if appRev.Spec.Application.Spec.Components[i].Name == comp.Name {
			revComponent = &appRev.Spec.Application.Spec.Components[i]
			break
		}
	}

	// First deployment or new component
	if revComponent == nil {
		return true
	}

	// Type changed
	if revComponent.Type != comp.Type {
		return true
	}

	// Compare properties as JSON to handle type normalization (e.g. int vs float64)
	currentProperties := comp.Params
	if currentProperties == nil {
		currentProperties = make(map[string]interface{})
	}

	currentJSON, err := json.Marshal(currentProperties)
	if err != nil {
		return true
	}

	var revJSON []byte
	if revComponent.Properties != nil && len(revComponent.Properties.Raw) > 0 {
		revJSON = revComponent.Properties.Raw
	} else {
		revJSON, _ = json.Marshal(map[string]interface{}{})
	}

	return !equality.Semantic.DeepEqual(currentJSON, revJSON)
}

// resolvedSourceHashes returns a per-source hash of the source values a
// component consumed during its most recent render (source name -> hash), and
// whether it consumed any. The resolved values live on comp.Ctx (populated by
// Complete() before dispatch); the raw spec comparison in
// componentPropertiesChanged cannot see them because comp.Params still holds the
// an unresolved expression.
func resolvedSourceHashes(comp *appfile.Component) (map[string]string, bool) {
	if comp == nil || comp.Ctx == nil {
		return nil, false
	}
	statuses, _ := comp.Ctx.GetData(definition.SourceResolutionStatusKey).(map[string]definition.SourceResolutionStatus)
	if len(statuses) == 0 {
		return nil, false
	}
	hashes := map[string]string{}
	for name, st := range statuses {
		if len(st.ConsumedFields) == 0 {
			continue
		}
		raw, err := json.Marshal(st.ConsumedFields) // json.Marshal sorts map keys
		if err != nil {
			continue
		}
		sum := sha256.Sum256(raw)
		hashes[name] = hex.EncodeToString(sum[:])
	}
	if len(hashes) == 0 {
		return nil, false
	}
	return hashes, true
}

// changedSources returns the names of consumed sources whose resolved value
// differs from what was last stamped on the live workload. A source missing
// from the live annotation (e.g. first apply, or a newly added source) counts
// as changed.
func changedSources(current map[string]string, live map[string]string) []string {
	var changed []string
	for name, h := range current {
		if live[name] != h {
			changed = append(changed, name)
		}
	}
	return changed
}

// stampResolvedSourceHashes records the per-source resolved hashes as a JSON
// annotation on the workload manifest so a later reconcile can detect a
// re-resolved value.
func stampResolvedSourceHashes(workload *unstructured.Unstructured, hashes map[string]string) {
	if workload == nil || len(hashes) == 0 {
		return
	}
	raw, err := json.Marshal(hashes)
	if err != nil {
		return
	}
	anns := workload.GetAnnotations()
	if anns == nil {
		anns = map[string]string{}
	}
	anns[oam.AnnotationSourceResolvedHash] = string(raw)
	workload.SetAnnotations(anns)
}

// liveResolvedSourceHashes reads the per-source resolved hashes previously
// stamped on the live workload in the target cluster. Returns an empty map when
// the workload is absent or carries no such annotation (so every current source
// counts as changed).
func liveResolvedSourceHashes(ctx context.Context, cli client.Client, clusterName string, workload *unstructured.Unstructured) map[string]string {
	if cli == nil || workload == nil {
		return nil
	}
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(workload.GroupVersionKind())
	getCtx := ctx
	if clusterName != "" {
		getCtx = pkgmulticluster.WithCluster(ctx, clusterName)
	}
	if err := cli.Get(getCtx, client.ObjectKey{Namespace: workload.GetNamespace(), Name: workload.GetName()}, live); err != nil {
		return nil
	}
	raw := live.GetAnnotations()[oam.AnnotationSourceResolvedHash]
	if raw == "" {
		return nil
	}
	hashes := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &hashes); err != nil {
		return nil
	}
	return hashes
}

// autoUpdateState is what an Application says about source-driven re-dispatch.
// It is a tri-state because "says nothing" and "says no" are different answers:
// the first defers to the controller-wide default, the second overrides it. A
// plain bool cannot carry that, which is why an Application had no way to opt
// out of a default that was on.
type autoUpdateState int

const (
	autoUpdateUnset autoUpdateState = iota
	autoUpdateOn
	autoUpdateOff
)

// enabled resolves the state against the controller-wide default that applies
// to Applications carrying no opinion of their own.
func (s autoUpdateState) enabled(defaultOn bool) bool {
	switch s {
	case autoUpdateOn:
		return true
	case autoUpdateOff:
		return false
	default:
		return defaultOn
	}
}

// sourceAutoUpdateSelector interprets the autoUpdate / autoUpdateSources
// annotations into a predicate over source names: matchAll=true means any
// consumed source triggers re-dispatch, otherwise only names in set do. Both
// are meaningless unless the returned state resolves to enabled.
//
// autoUpdateSources wins over autoUpdate when both are present. It is the
// narrower and therefore the more deliberate statement, so an Application that
// is autoUpdate: "true" can still turn source-driven re-dispatch off without
// giving up auto-update everywhere else.
//
// The off words are enumerated rather than derived as "not true" because
// anything unrecognised here is a source binding name, and binding names are
// arbitrary. A typo like "flase" is therefore a source name that matches
// nothing rather than an error - the webhook reports a listed name that is not
// declared in spec.sources. The cost is that a binding literally named "true",
// "false", "off", "none" or "*" cannot be selected on its own.
func sourceAutoUpdateSelector(annotations map[string]string) (matchAll bool, set map[string]struct{}, state autoUpdateState) {
	raw, present := annotations[oam.AnnotationAutoUpdateSources]
	if present {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "true", "*":
			return true, nil, autoUpdateOn
		case "false", "off", "none":
			return false, nil, autoUpdateOff
		}
		set = map[string]struct{}{}
		for _, name := range strings.Split(raw, ",") {
			if n := strings.TrimSpace(name); n != "" {
				set[n] = struct{}{}
			}
		}
		if len(set) > 0 {
			return false, set, autoUpdateOn
		}
		// Present but empty, or nothing but separators. The annotation was
		// written deliberately, so it is an explicit no rather than silence.
		return false, nil, autoUpdateOff
	}
	if annotations[oam.AnnotationAutoUpdate] == "true" {
		return true, nil, autoUpdateOn
	}
	return false, nil, autoUpdateUnset
}
