/*
Copyright 2021 The KubeVela Authors.

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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"

	"github.com/oam-dev/kubevela/pkg/cue/definition/health"
	"github.com/oam-dev/kubevela/pkg/definition/cachekey"
	"github.com/oam-dev/kubevela/pkg/features"

	upstreamcuex "github.com/kubevela/pkg/cue/cuex"
	velacuex "github.com/oam-dev/kubevela/pkg/cue/cuex"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/ast"
	"cuelang.org/go/cue/cuecontext"
	cueerrors "cuelang.org/go/cue/errors"
	cueformat "cuelang.org/go/cue/format"
	cueparser "cuelang.org/go/cue/parser"
	"github.com/kubevela/pkg/multicluster"

	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubevela/workflow/pkg/cue/model"
	"github.com/kubevela/workflow/pkg/cue/model/sets"
	"github.com/kubevela/workflow/pkg/cue/model/value"
	"github.com/kubevela/workflow/pkg/cue/process"

	apitypes "github.com/oam-dev/kubevela/apis/types"
	velaprocess "github.com/oam-dev/kubevela/pkg/cue/process"
	"github.com/oam-dev/kubevela/pkg/cue/task"
	"github.com/oam-dev/kubevela/pkg/cue/upgrade"
	"github.com/oam-dev/kubevela/pkg/oam"
	"github.com/oam-dev/kubevela/pkg/oam/util"
)

const (
	// OutputFieldName is the name of the struct contains the CR data
	OutputFieldName = velaprocess.OutputFieldName
	// OutputsFieldName is the name of the struct contains the map[string]CR data
	OutputsFieldName = velaprocess.OutputsFieldName
	// PatchFieldName is the name of the struct contains the patch of CR data
	PatchFieldName = "patch"
	// PatchOutputsFieldName is the name of the struct contains the patch of outputs CR data
	PatchOutputsFieldName = "patchOutputs"
	// ErrsFieldName check if errors contained in the cue
	ErrsFieldName = "errs"
	// TemplateContextPrefix is the base prefix for storing templates in context
	TemplateContextPrefix = "template-context-"
	// SourceResolutionStatusKey stores per-source runtime resolution statuses in process context.
	SourceResolutionStatusKey = "sourceResolutionStatuses"
	sourceCacheNamespace      = "vela-system"
	sourceCacheTTL            = 15 * time.Minute
	sourceCacheSyncAtKey      = apitypes.AnnotationConfigLastSyncAt
	sourceCacheAccessedKey    = apitypes.AnnotationConfigLastAccessed
	sourceCacheTTLKey         = apitypes.AnnotationConfigTTL
	sourceCacheTemplateKey    = apitypes.AnnotationConfigTemplate
	sourceCacheDataKey        = "input-properties"
	sourceCachePolicyUseStale = "use-stale"
	sourceCachePolicyFail     = "fail"
)

type sourceCachePolicy struct {
	Key            string
	TTL            time.Duration
	OnStaleFailure string
}

// GetWorkloadTemplateKey returns the context key for storing workload templates
func GetWorkloadTemplateKey(name string) string {
	return TemplateContextPrefix + "workload-" + name
}

// GetTraitTemplateKey returns the context key for storing trait templates
func GetTraitTemplateKey(name string) string {
	return TemplateContextPrefix + "trait-" + name
}

const (
	// AuxiliaryWorkload defines the extra workload obj from a workloadDefinition,
	// e.g. a workload composed by deployment and service, the service will be marked as AuxiliaryWorkload
	AuxiliaryWorkload = "AuxiliaryWorkload"
)

// AbstractEngine defines Definition's Render interface
type AbstractEngine interface {
	Complete(ctx process.Context, abstractTemplate string, params interface{}) error
	Status(templateContext map[string]interface{}, request *health.StatusRequest) (*health.StatusResult, error)
	GetTemplateContext(ctx process.Context, cli client.Client, accessor util.NamespaceAccessor) (map[string]interface{}, error)
}

type def struct {
	name string
}

type workloadDef struct {
	def
}

// NewWorkloadAbstractEngine create Workload Definition AbstractEngine
func NewWorkloadAbstractEngine(name string) AbstractEngine {
	return &workloadDef{
		def: def{
			name: name,
		},
	}
}

// Complete do workload definition's rendering
func (wd *workloadDef) Complete(ctx process.Context, abstractTemplate string, params interface{}) (retErr error) {
	start := time.Now()
	defer func() {
		status := "ok"
		if retErr != nil {
			status = "error"
		}
		CUERenderDuration.WithLabelValues(string(upgrade.ComponentKind), status).Observe(time.Since(start).Seconds())
	}()

	var paramFile = velaprocess.ParameterFieldName + ": {}"
	if params != nil {
		resolved, err := resolveFromSourceParams(ctx, params)
		if err != nil {
			return errors.WithMessagef(err, "resolve fromSource for workload %s", wd.name)
		}
		bt, err := json.Marshal(params)
		if resolved != nil {
			bt, err = json.Marshal(resolved)
		}
		if err != nil {
			return errors.WithMessagef(err, "marshal parameter of workload %s", wd.name)
		}
		if string(bt) != "null" {
			paramFile = fmt.Sprintf("%s: %s", velaprocess.ParameterFieldName, string(bt))
		}
	}

	c, err := ctx.BaseContextFile()
	if err != nil {
		return err
	}

	abstractTemplate, _ = upgrade.EnsureCueVersionCompatibility(abstractTemplate, wd.name, upgrade.ComponentKind, upgrade.TemplateAreaMain)

	val, err := velacuex.WorkloadCompiler.Get().CompileString(ctx.GetCtx(), strings.Join([]string{
		renderTemplate(abstractTemplate), paramFile, c,
	}, "\n"))
	if err != nil {
		return errors.WithMessagef(err, "failed to compile workload %s after merge parameter and context", wd.name)
	}

	userErrors := extractUserErrors(val, "Workload definition", wd.name)

	validationErr := val.Validate()

	if validationErr != nil || len(userErrors) > 0 {
		var result strings.Builder
		result.WriteString(fmt.Sprintf("validation failed for workload %s:", wd.name))

		if len(userErrors) > 0 {
			result.WriteString("\n\nUser Errors:\n")
			for _, e := range userErrors {
				result.WriteString(fmt.Sprintf("  %s\n", e))
			}
		}

		if validationErr != nil {
			if fmtErr := FormatCUEError(validationErr, "validation failed for", "workload", wd.name, &val); fmtErr != nil {
				errMsg := fmtErr.Error()
				errMsg = strings.TrimPrefix(errMsg, fmt.Sprintf("validation failed for workload %s:", wd.name))
				result.WriteString(errMsg)
			}
		}

		return errors.New(strings.TrimRight(result.String(), "\n"))
	}
	output := val.LookupPath(value.FieldPath(OutputFieldName))

	base, err := model.NewBase(output)
	if err != nil {
		return errors.WithMessagef(err, "invalid output of workload %s", wd.name)
	}
	if err := ctx.SetBase(base); err != nil {
		return err
	}

	// Store template for error context (use workload-specific key to avoid pollution)
	ctx.PushData(GetWorkloadTemplateKey(wd.name), val)

	// we will support outputs for workload composition, and it will become trait in AppConfig.
	outputs := val.LookupPath(value.FieldPath(OutputsFieldName))
	if !outputs.Exists() {
		return nil
	}

	iter, err := outputs.Fields(cue.Definitions(true), cue.Hidden(true), cue.All())
	if err != nil {
		return errors.WithMessagef(err, "invalid outputs of workload %s", wd.name)
	}
	for iter.Next() {
		if iter.Selector().IsDefinition() || iter.Selector().PkgPath() != "" || iter.IsOptional() {
			continue
		}
		other, err := model.NewOther(iter.Value())
		name := util.GetIteratorLabel(*iter)
		if err != nil {
			return errors.WithMessagef(err, "invalid outputs(%s) of workload %s", name, wd.name)
		}
		if err := ctx.AppendAuxiliaries(process.Auxiliary{Ins: other, Type: AuxiliaryWorkload, Name: name}); err != nil {
			return err
		}
	}
	return nil
}

func withCluster(ctx context.Context, o client.Object) context.Context {
	if cluster := oam.GetCluster(o); cluster != "" {
		return multicluster.WithCluster(ctx, cluster)
	}
	return ctx
}

func (wd *workloadDef) getTemplateContext(ctx process.Context, cli client.Reader, accessor util.NamespaceAccessor) (map[string]interface{}, error) {
	baseLabels := GetBaseContextLabels(ctx)
	var root = initRoot(baseLabels)
	var commonLabels = GetCommonLabels(baseLabels)

	base, assists := ctx.Output()
	componentWorkload, err := base.Unstructured()
	if err != nil {
		return nil, err
	}
	// workload main resource will have a unique label("app.oam.dev/resourceType"="WORKLOAD") in per component/app level
	_ctx := withCluster(ctx.GetCtx(), componentWorkload)
	object, err := getResourceFromObj(_ctx, ctx, componentWorkload, cli, accessor.For(componentWorkload), util.MergeMapOverrideWithDst(map[string]string{
		oam.LabelOAMResourceType: oam.ResourceTypeWorkload,
	}, commonLabels), "")
	if err != nil {
		return nil, err
	}
	root[OutputFieldName] = object
	outputs := make(map[string]interface{})
	for _, assist := range assists {
		if assist.Type != AuxiliaryWorkload {
			continue
		}
		if assist.Name == "" {
			return nil, errors.New("the auxiliary of workload must have a name with format 'outputs.<my-name>'")
		}
		traitRef, err := assist.Ins.Unstructured()
		if err != nil {
			return nil, err
		}
		// AuxiliaryWorkload will have a unique label("trait.oam.dev/resource"="name of outputs") in per component/app level
		_ctx := withCluster(ctx.GetCtx(), traitRef)
		object, err := getResourceFromObj(_ctx, ctx, traitRef, cli, accessor.For(traitRef), util.MergeMapOverrideWithDst(map[string]string{
			oam.TraitTypeLabel: AuxiliaryWorkload,
		}, commonLabels), assist.Name)
		if err != nil {
			return nil, err
		}
		outputs[assist.Name] = object
	}
	if len(outputs) > 0 {
		root[OutputsFieldName] = outputs
	}
	return root, nil
}

// Status get workload status by customStatusTemplate
func (wd *workloadDef) Status(templateContext map[string]interface{}, request *health.StatusRequest) (*health.StatusResult, error) {
	return health.GetStatus(templateContext, request)
}

func (wd *workloadDef) GetTemplateContext(ctx process.Context, cli client.Client, accessor util.NamespaceAccessor) (map[string]interface{}, error) {
	return wd.getTemplateContext(ctx, cli, accessor)
}

type traitDef struct {
	def
}

// NewTraitAbstractEngine create Trait Definition AbstractEngine
func NewTraitAbstractEngine(name string) AbstractEngine {
	return &traitDef{
		def: def{
			name: name,
		},
	}
}

// Complete do trait definition's rendering
// nolint:gocyclo
func (td *traitDef) Complete(ctx process.Context, abstractTemplate string, params interface{}) (retErr error) {
	start := time.Now()
	defer func() {
		status := "ok"
		if retErr != nil {
			status = "error"
		}
		CUERenderDuration.WithLabelValues(string(upgrade.TraitKind), status).Observe(time.Since(start).Seconds())
	}()

	abstractTemplate, _ = upgrade.EnsureCueVersionCompatibility(abstractTemplate, td.name, upgrade.TraitKind, upgrade.TemplateAreaMain)
	buff := abstractTemplate + "\n"
	if params != nil {
		resolved, err := resolveFromSourceParams(ctx, params)
		if err != nil {
			return errors.WithMessagef(err, "resolve fromSource for trait %s", td.name)
		}
		bt, err := json.Marshal(params)
		if resolved != nil {
			bt, err = json.Marshal(resolved)
		}
		if err != nil {
			return errors.WithMessagef(err, "marshal parameter of trait %s", td.name)
		}
		if string(bt) != "null" {
			buff += fmt.Sprintf("%s: %s\n", velaprocess.ParameterFieldName, string(bt))
		}
	}

	multiStageEnabled := feature.DefaultMutableFeatureGate.Enabled(features.MultiStageComponentApply)
	var statusBytes []byte
	if multiStageEnabled {
		statusBytes = outputStatusBytes(ctx)
	}

	c, err := ctx.BaseContextFile()
	if err != nil {
		return err
	}

	// When multi-stage is enabled, merge the existing output.status from ctx into the
	// base context so downstream CUE can reference it deterministically.
	if multiStageEnabled {
		c = injectOutputStatusIntoBaseContext(ctx, c, statusBytes)
	}

	buff += c

	val, err := velacuex.WorkloadCompiler.Get().CompileString(ctx.GetCtx(), buff)

	if err != nil {
		return errors.WithMessagef(err, "failed to compile trait %s after merge parameter and context", td.name)
	}

	userErrors := extractUserErrors(val, "Trait definition", td.name)

	validationErr := val.Validate()

	if validationErr != nil || len(userErrors) > 0 {
		var result strings.Builder
		result.WriteString(fmt.Sprintf("validation failed for trait %s:", td.name))

		if len(userErrors) > 0 {
			result.WriteString("\n\nUser Errors:\n")
			for _, e := range userErrors {
				result.WriteString(fmt.Sprintf("  %s\n", e))
			}
		}

		if validationErr != nil {
			if fmtErr := FormatCUEError(validationErr, "validation failed for", "trait", td.name, &val); fmtErr != nil {
				errMsg := fmtErr.Error()
				errMsg = strings.TrimPrefix(errMsg, fmt.Sprintf("validation failed for trait %s:", td.name))
				result.WriteString(errMsg)
			}
		}

		return errors.New(strings.TrimRight(result.String(), "\n"))
	}

	processing := val.LookupPath(value.FieldPath("processing"))
	if processing.Exists() {
		if val, err = task.Process(val); err != nil {
			return errors.WithMessagef(err, "invalid process of trait %s", td.name)
		}
	}
	outputs := val.LookupPath(value.FieldPath(OutputsFieldName))
	if outputs.Exists() {

		iter, err := outputs.Fields(cue.Definitions(true), cue.Hidden(true), cue.All())
		if err != nil {
			return errors.WithMessagef(err, "invalid outputs of trait %s", td.name)
		}
		for iter.Next() {
			if iter.Selector().IsDefinition() || iter.Selector().PkgPath() != "" || iter.IsOptional() {
				continue
			}
			other, err := model.NewOther(iter.Value())
			name := util.GetIteratorLabel(*iter)
			if err != nil {
				return errors.WithMessagef(err, "invalid outputs(resource=%s) of trait %s", name, td.name)
			}
			if err := ctx.AppendAuxiliaries(process.Auxiliary{Ins: other, Type: td.name, Name: name}); err != nil {
				return err
			}
		}
	}

	patcher := val.LookupPath(value.FieldPath(PatchFieldName))
	base, auxiliaries := ctx.Output()
	if patcher.Exists() {
		if base == nil {
			return fmt.Errorf("patch trait %s into an invalid workload", td.name)
		}
		if err := base.Unify(patcher, sets.CreateUnifyOptionsForPatcher(patcher)...); err != nil {
			return errors.WithMessagef(err, "invalid patch trait %s into workload", td.name)
		}
	}
	outputsPatcher := val.LookupPath(value.FieldPath(PatchOutputsFieldName))
	if outputsPatcher.Exists() {
		for _, auxiliary := range auxiliaries {
			target := outputsPatcher.LookupPath(value.FieldPath(auxiliary.Name))
			if !target.Exists() {
				continue
			}
			if err = auxiliary.Ins.Unify(target); err != nil {
				return errors.WithMessagef(err, "trait=%s, to=%s, invalid patch trait into auxiliary workload", td.name, auxiliary.Name)
			}
		}
	}

	return nil
}

func outputStatusBytes(ctx process.Context) []byte {
	var statusBytes []byte
	var outputMap map[string]interface{}
	if output := ctx.GetData(OutputFieldName); output != nil {
		if m, ok := output.(map[string]interface{}); ok {
			outputMap = m
		} else if ptr, ok := output.(*interface{}); ok && ptr != nil {
			if m, ok := (*ptr).(map[string]interface{}); ok {
				outputMap = m
			}
		}

		if outputMap != nil {
			if status, ok := outputMap["status"]; ok {
				if b, err := json.Marshal(status); err == nil {
					statusBytes = b
				}
			}
		}
	}
	return statusBytes
}

func injectOutputStatusIntoBaseContext(ctx process.Context, c string, statusBytes []byte) string {
	if len(statusBytes) > 0 {
		// If output is an empty object, replace it with only the status field without trailing comma.
		emptyOutputMarker := "\"output\":{}"
		if strings.Contains(c, emptyOutputMarker) {
			replacement := fmt.Sprintf("\"output\":{\"status\":%s}", string(statusBytes))
			c = strings.Replace(c, emptyOutputMarker, replacement, 1)
		} else {
			// Otherwise, insert status as the first field and keep the comma to separate from existing fields.
			replacement := fmt.Sprintf("\"output\":{\"status\":%s,", string(statusBytes))
			c = strings.Replace(c, "\"output\":{", replacement, 1)
		}

		// Restore the status field to the current output in ctx.data
		var status interface{}
		if err := json.Unmarshal(statusBytes, &status); err == nil {
			if currentOutput := ctx.GetData(OutputFieldName); currentOutput != nil {
				if currentMap, ok := currentOutput.(map[string]interface{}); ok {
					currentMap["status"] = status
					ctx.PushData(OutputFieldName, currentMap)
				}
			}
		}
	}
	return c
}

// GetCommonLabels will convert context based labels to OAM standard labels
func GetCommonLabels(contextLabels map[string]string) map[string]string {
	var commonLabels = map[string]string{}
	for k, v := range contextLabels {
		switch k {
		case velaprocess.ContextAppName:
			commonLabels[oam.LabelAppName] = v
		case velaprocess.ContextName:
			commonLabels[oam.LabelAppComponent] = v
		case velaprocess.ContextAppRevision:
			commonLabels[oam.LabelAppRevision] = v
		case velaprocess.ContextReplicaKey:
			commonLabels[oam.LabelReplicaKey] = v

		}
	}
	return commonLabels
}

// GetBaseContextLabels get base context labels
func GetBaseContextLabels(ctx process.Context) map[string]string {
	baseLabels := ctx.BaseContextLabels()
	baseLabels[velaprocess.ContextAppName] = ctx.GetData(velaprocess.ContextAppName).(string)
	baseLabels[velaprocess.ContextAppRevision] = ctx.GetData(velaprocess.ContextAppRevision).(string)

	return baseLabels
}

func initRoot(contextLabels map[string]string) map[string]interface{} {
	var root = map[string]interface{}{}
	for k, v := range contextLabels {
		root[k] = v
	}
	return root
}

func renderTemplate(templ string) string {
	return templ + `
context: _
parameter: _
`
}

func (td *traitDef) getTemplateContext(ctx process.Context, cli client.Reader, accessor util.NamespaceAccessor) (map[string]interface{}, error) {
	baseLabels := GetBaseContextLabels(ctx)
	var root = initRoot(baseLabels)
	var commonLabels = GetCommonLabels(baseLabels)
	_, assists := ctx.Output()

	outputs := make(map[string]interface{})
	for _, assist := range assists {
		if assist.Type != td.name {
			continue
		}
		traitRef, err := assist.Ins.Unstructured()
		if err != nil {
			return nil, err
		}
		_ctx := withCluster(ctx.GetCtx(), traitRef)
		object, err := getResourceFromObj(_ctx, ctx, traitRef, cli, accessor.For(traitRef), util.MergeMapOverrideWithDst(map[string]string{
			oam.TraitTypeLabel: assist.Type,
		}, commonLabels), assist.Name)
		if err != nil {
			return nil, err
		}
		outputs[assist.Name] = object
	}
	if len(outputs) > 0 {
		root[OutputsFieldName] = outputs
	}
	return root, nil
}

// Status get trait status by customStatusTemplate
func (td *traitDef) Status(templateContext map[string]interface{}, request *health.StatusRequest) (*health.StatusResult, error) {
	return health.GetStatus(templateContext, request)
}

func (td *traitDef) GetTemplateContext(ctx process.Context, cli client.Client, accessor util.NamespaceAccessor) (map[string]interface{}, error) {
	return td.getTemplateContext(ctx, cli, accessor)
}

func getResourceFromObj(ctx context.Context, pctx process.Context, obj *unstructured.Unstructured, client client.Reader, namespace string, labels map[string]string, outputsResource string) (map[string]interface{}, error) {
	if outputsResource != "" {
		labels[oam.TraitResource] = outputsResource
	}
	if obj.GetName() != "" {
		u, err := util.GetObjectGivenGVKAndName(ctx, client, obj.GroupVersionKind(), namespace, obj.GetName())
		if err != nil {
			return nil, err
		}
		return u.Object, nil
	}
	if ctxName := pctx.GetData(model.ContextName).(string); ctxName != "" {
		u, err := util.GetObjectGivenGVKAndName(ctx, client, obj.GroupVersionKind(), namespace, ctxName)
		if err == nil {
			return u.Object, nil
		}
	}
	list, err := util.GetObjectsGivenGVKAndLabels(ctx, client, obj.GroupVersionKind(), namespace, labels)
	if err != nil {
		return nil, err
	}
	if len(list.Items) == 1 {
		return list.Items[0].Object, nil
	}
	for _, v := range list.Items {
		if v.GetLabels()[oam.TraitResource] == outputsResource {
			return v.Object, nil
		}
	}
	return nil, errors.Errorf("no resources found gvk(%v) labels(%v)", obj.GroupVersionKind(), labels)
}

// FormatCUEError formats CUE errors in a user-friendly grouped format
func FormatCUEError(err error, messagePrefix string, entityType, entityName string, val ...*cue.Value) error {
	var allParamErrors = make(map[string]bool)
	var allTemplateErrors = make(map[string]bool)

	if err != nil {
		errList := cueerrors.Errors(err)
		for _, e := range errList {
			errMsg := e.Error()
			if strings.HasPrefix(errMsg, "parameter.") {
				allParamErrors[errMsg] = true
			} else {
				allTemplateErrors[errMsg] = true
			}
		}

		if len(val) > 0 && val[0] != nil {
			if concreteErr := val[0].Validate(cue.Concrete(true)); concreteErr != nil {
				concreteErrList := cueerrors.Errors(concreteErr)
				for _, e := range concreteErrList {
					errMsg := e.Error()
					if strings.HasPrefix(errMsg, "parameter.") {
						allParamErrors[errMsg] = true
					} else {
						allTemplateErrors[errMsg] = true
					}
				}
			}
		}
	}

	if len(allParamErrors) == 0 && len(allTemplateErrors) == 0 {
		return nil
	}

	var result strings.Builder
	result.WriteString(fmt.Sprintf("%s %s %s:", messagePrefix, entityType, entityName))

	if len(allParamErrors) > 0 {
		result.WriteString("\n\nParameter errors:\n")
		// Sort errors for deterministic output
		paramErrs := make([]string, 0, len(allParamErrors))
		for errMsg := range allParamErrors {
			paramErrs = append(paramErrs, errMsg)
		}
		sort.Strings(paramErrs)
		for _, errMsg := range paramErrs {
			result.WriteString("  " + errMsg + "\n")
		}
	}

	if len(allTemplateErrors) > 0 {
		result.WriteString("\n\nTemplate errors:\n")
		templateErrs := make([]string, 0, len(allTemplateErrors))
		for errMsg := range allTemplateErrors {
			templateErrs = append(templateErrs, errMsg)
		}
		sort.Strings(templateErrs)
		for _, errMsg := range templateErrs {
			result.WriteString("  " + errMsg + "\n")
		}
	}

	return fmt.Errorf("%s", strings.TrimRight(result.String(), "\n"))
}

func resolveFromSourceParams(ctx process.Context, params interface{}) (interface{}, error) {
	if params == nil {
		return nil, nil
	}
	bt, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	var normalized interface{}
	if err := json.Unmarshal(bt, &normalized); err != nil {
		return nil, err
	}
	return resolveFromSourceNode(normalized, newSourceResolver(ctx))
}

func resolveFromSourceNode(node interface{}, resolver *sourceResolver) (interface{}, error) {
	switch val := node.(type) {
	case map[string]interface{}:
		if selector, ok := val["fromSource"]; ok {
			return evaluateFromSourceSelector(selector, resolver)
		}
		for k, child := range val {
			resolved, err := resolveFromSourceNode(child, resolver)
			if err != nil {
				return nil, err
			}
			val[k] = resolved
		}
		return val, nil
	case []interface{}:
		for i, child := range val {
			resolved, err := resolveFromSourceNode(child, resolver)
			if err != nil {
				return nil, err
			}
			val[i] = resolved
		}
		return val, nil
	default:
		return node, nil
	}
}

func evaluateFromSourceSelector(selector interface{}, resolver *sourceResolver) (interface{}, error) {
	sourceName := ""
	path := ""
	var def interface{}
	switch v := selector.(type) {
	case string:
		parts := strings.SplitN(v, ".", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid fromSource reference %q", v)
		}
		sourceName, path = parts[0], parts[1]
	case map[string]interface{}:
		if n, ok := v["name"].(string); ok {
			sourceName = n
		}
		if p, ok := v["path"].(string); ok {
			path = p
		}
		def = v["default"]
	default:
		return nil, fmt.Errorf("invalid fromSource selector type %T", selector)
	}
	if sourceName == "" || path == "" {
		return nil, fmt.Errorf("fromSource requires source name and path")
	}
	sourceVals, err := resolver.resolve(sourceName)
	if err != nil {
		if def != nil {
			return def, nil
		}
		return nil, err
	}
	if val, ok := lookupMapPath(sourceVals, path); ok {
		resolver.recordConsumedValue(sourceName, resolver.sourceTypes[sourceName], path, val)
		return val, nil
	}
	if def != nil {
		return def, nil
	}
	return nil, fmt.Errorf("path %q not found in source %q", path, sourceName)
}

func lookupMapPath(data map[string]interface{}, path string) (interface{}, bool) {
	cur := interface{}(data)
	for _, p := range strings.Split(path, ".") {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		next, ok := m[p]
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

type sourceResolver struct {
	ctx             process.Context
	sourceProps     map[string]map[string]interface{}
	sourceTypes     map[string]string
	sourceTemplates map[string]string
	sourceSchemas   map[string]string
	sensitivePaths  map[string][]string
	cacheStore      velaprocess.SourceCacheStore
	resolved        map[string]map[string]interface{}
	resolving       map[string]bool
}

// SourceResolutionStatus captures source runtime resolution result.
type SourceResolutionStatus struct {
	Name           string
	Type           string
	Phase          string
	Message        string
	Config         string
	ExpiresAt      string
	ResolvedFields map[string]interface{}
	ConsumedFields map[string]interface{}
	SensitivePaths []string
}

func newSourceResolver(ctx process.Context) *sourceResolver {
	sourceProps, _ := ctx.GetData(velaprocess.ContextAppSources).(map[string]map[string]interface{})
	if sourceProps == nil {
		sourceProps = map[string]map[string]interface{}{}
	}
	sourceTypes, _ := ctx.GetData(velaprocess.ContextAppSourceTypes).(map[string]string)
	if sourceTypes == nil {
		sourceTypes = map[string]string{}
	}
	sourceTemplates, _ := ctx.GetData(velaprocess.ContextAppSourceTemplates).(map[string]string)
	if sourceTemplates == nil {
		sourceTemplates = map[string]string{}
	}
	sourceSchemas := map[string]string{}
	for sourceType, sourceTemplate := range sourceTemplates {
		schemaExpr, err := extractSourceSchemaExpr(sourceTemplate)
		if err != nil {
			klog.Warningf("extract source schema failed for %s: %v", sourceType, err)
			continue
		}
		if schemaExpr != "" {
			sourceSchemas[sourceType] = schemaExpr
		}
	}
	sensitivePaths, _ := ctx.GetData(velaprocess.ContextAppSourceSensitivePaths).(map[string][]string)
	if sensitivePaths == nil {
		sensitivePaths = map[string][]string{}
	}
	var cacheStore velaprocess.SourceCacheStore
	if s, ok := ctx.GetData(velaprocess.ContextAppSourceCacheStore).(velaprocess.SourceCacheStore); ok && s != nil {
		cacheStore = s
	}
	return &sourceResolver{
		ctx:             ctx,
		sourceProps:     sourceProps,
		sourceTypes:     sourceTypes,
		sourceTemplates: sourceTemplates,
		sourceSchemas:   sourceSchemas,
		sensitivePaths:  sensitivePaths,
		cacheStore:      cacheStore,
		resolved:        map[string]map[string]interface{}{},
		resolving:       map[string]bool{},
	}
}

// extractUserErrors reads the authored `errs:` field ([]string) from a compiled
// CUE value and returns its non-empty entries. A malformed `errs:` field is
// logged and treated as empty so error reporting never masks the real result.
func extractUserErrors(val cue.Value, entityType, entityName string) []string {
	errs := val.LookupPath(value.FieldPath(ErrsFieldName))
	if !errs.Exists() {
		return nil
	}
	var userErrors []string
	if err := errs.Decode(&userErrors); err != nil {
		klog.Warningf("%s '%s' has malformed 'errs' field (expected []string): %v. Custom error reporting will be skipped.", entityType, entityName, err)
		return nil
	}
	filtered := userErrors[:0]
	for _, e := range userErrors {
		if strings.TrimSpace(e) != "" {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

func (r *sourceResolver) resolve(sourceName string) (map[string]interface{}, error) {
	if v, ok := r.resolved[sourceName]; ok {
		return v, nil
	}
	if r.resolving[sourceName] {
		err := fmt.Errorf("circular source dependency detected at %q", sourceName)
		r.setSourceStatus(sourceName, "", "Failed", err.Error(), "", "", nil)
		return nil, err
	}
	r.resolving[sourceName] = true
	defer delete(r.resolving, sourceName)

	sourceType, ok := r.sourceTypes[sourceName]
	if !ok || sourceType == "" {
		err := fmt.Errorf("source %q not found", sourceName)
		r.setSourceStatus(sourceName, "", "Failed", err.Error(), "", "", nil)
		return nil, err
	}
	sourceTemplate, ok := r.sourceTemplates[sourceType]
	if !ok || sourceTemplate == "" {
		err := fmt.Errorf("source definition %q for source %q is missing cue template", sourceType, sourceName)
		r.setSourceStatus(sourceName, sourceType, "Failed", err.Error(), "", "", nil)
		return nil, err
	}
	resolvedProps := map[string]interface{}{}
	paramFile := velaprocess.ParameterFieldName + ": {}"
	if props, ok := r.sourceProps[sourceName]; ok && props != nil {
		resolvedPropsNode, err := resolveFromSourceNode(props, r)
		if err != nil {
			r.setSourceStatus(sourceName, sourceType, "Failed", err.Error(), "", "", nil)
			return nil, errors.WithMessagef(err, "resolve source properties for %s", sourceName)
		}
		rp, ok := resolvedPropsNode.(map[string]interface{})
		if !ok {
			err := fmt.Errorf("resolved source properties for %s are invalid", sourceName)
			r.setSourceStatus(sourceName, sourceType, "Failed", err.Error(), "", "", nil)
			return nil, err
		}
		resolvedProps = rp
		raw, err := json.Marshal(rp)
		if err != nil {
			r.setSourceStatus(sourceName, sourceType, "Failed", err.Error(), "", "", nil)
			return nil, errors.WithMessagef(err, "marshal properties for source %s", sourceName)
		}
		paramFile = fmt.Sprintf("%s: %s", velaprocess.ParameterFieldName, string(raw))
	}
	cachePolicy, err := r.resolveCachePolicy(sourceName, sourceType, sourceTemplate, resolvedProps)
	if err != nil {
		r.setSourceStatus(sourceName, sourceType, "Failed", err.Error(), "", "", nil)
		return nil, err
	}
	// storage.key covers the context this source reads; the properties it was
	// bound with are hashed in here. Without that, two bindings differing only in
	// their properties would address the same entry and the second would receive
	// the first's value.
	cachePolicy.Key, err = cacheIdentity(cachePolicy.Key, resolvedProps)
	if err != nil {
		r.setSourceStatus(sourceName, sourceType, "Failed", err.Error(), "", "", nil)
		return nil, err
	}
	cached, stale, found, cacheExpiresAt, err := r.readSourceCache(cachePolicy.Key, cachePolicy.TTL)
	if err != nil {
		klog.Warningf("read source cache failed for %s: %v", sourceName, err)
	} else if found {
		if !stale {
			r.resolved[sourceName] = cached
			r.setSourceStatus(sourceName, sourceType, "Resolved", "", cachePolicy.Key, cacheExpiresAt.Format(time.RFC3339), cached)
			return cached, nil
		}
	}
	// A source is compiled against the context the cache-key rules make readable,
	// not the component's - so it cannot depend on anything the key ignores.
	c, err := sourceContext(r.ctx, sourceName)
	if err != nil {
		r.setSourceStatus(sourceName, sourceType, "Failed", err.Error(), cachePolicy.Key, "", nil)
		return nil, err
	}
	val, err := velacuex.WorkloadCompiler.Get().CompileString(r.ctx.GetCtx(), strings.Join([]string{
		renderTemplate(sourceTemplate), paramFile, c,
	}, "\n"))
	if err != nil {
		if found && stale && cachePolicy.OnStaleFailure == sourceCachePolicyUseStale {
			r.touchSourceCache(cachePolicy.Key)
			r.resolved[sourceName] = cached
			r.setSourceStatus(sourceName, sourceType, "Resolved", "refresh failed; serving stale cached value", cachePolicy.Key, cacheExpiresAt.Format(time.RFC3339), cached)
			return cached, nil
		}
		r.setSourceStatus(sourceName, sourceType, "Failed", err.Error(), cachePolicy.Key, "", nil)
		return nil, errors.WithMessagef(err, "compile source definition %s", sourceType)
	}
	if userErrs := extractUserErrors(val, "source definition", sourceType); len(userErrs) > 0 {
		errMsg := strings.Join(userErrs, "; ")
		if found && stale && cachePolicy.OnStaleFailure == sourceCachePolicyUseStale {
			r.touchSourceCache(cachePolicy.Key)
			r.resolved[sourceName] = cached
			r.setSourceStatus(sourceName, sourceType, "Resolved", "refresh reported errors; serving stale cached value", cachePolicy.Key, cacheExpiresAt.Format(time.RFC3339), cached)
			return cached, nil
		}
		r.setSourceStatus(sourceName, sourceType, "Failed", errMsg, cachePolicy.Key, "", nil)
		return nil, fmt.Errorf("source definition %s reported errors: %s", sourceType, errMsg)
	}
	output := map[string]interface{}{}
	if err := val.LookupPath(value.FieldPath(OutputFieldName)).Decode(&output); err != nil {
		if found && stale && cachePolicy.OnStaleFailure == sourceCachePolicyUseStale {
			r.touchSourceCache(cachePolicy.Key)
			r.resolved[sourceName] = cached
			r.setSourceStatus(sourceName, sourceType, "Resolved", "refresh failed; serving stale cached value", cachePolicy.Key, cacheExpiresAt.Format(time.RFC3339), cached)
			return cached, nil
		}
		r.setSourceStatus(sourceName, sourceType, "Failed", err.Error(), cachePolicy.Key, "", nil)
		return nil, errors.WithMessagef(err, "decode output for source definition %s", sourceType)
	}
	if err := r.validateResolvedOutput(sourceType, sourceTemplate, output); err != nil {
		if found && stale && cachePolicy.OnStaleFailure == sourceCachePolicyUseStale {
			r.touchSourceCache(cachePolicy.Key)
			r.resolved[sourceName] = cached
			r.setSourceStatus(sourceName, sourceType, "Resolved", "refresh failed; serving stale cached value", cachePolicy.Key, cacheExpiresAt.Format(time.RFC3339), cached)
			return cached, nil
		}
		r.setSourceStatus(sourceName, sourceType, "Failed", err.Error(), cachePolicy.Key, "", nil)
		return nil, errors.WithMessagef(err, "validate output against schema for source definition %s", sourceType)
	}
	r.resolved[sourceName] = output
	expiresAt := time.Now().Add(cachePolicy.TTL).Format(time.RFC3339)
	if err := r.writeSourceCache(cachePolicy.Key, sourceType, output, cachePolicy.TTL); err != nil {
		klog.Warningf("write source cache failed for %s: %v", sourceName, err)
	}
	r.setSourceStatus(sourceName, sourceType, "Resolved", "", cachePolicy.Key, expiresAt, output)
	return output, nil
}

func (r *sourceResolver) resolveCachePolicy(sourceName, sourceType, sourceTemplate string, props map[string]interface{}) (sourceCachePolicy, error) {
	policy := sourceCachePolicy{
		TTL:            sourceCacheTTL,
		OnStaleFailure: sourceCachePolicyUseStale,
	}
	if sourceTemplate == "" {
		return policy, fmt.Errorf("source definition %q has no cue template", sourceType)
	}
	paramFile := velaprocess.ParameterFieldName + ": {}"
	if len(props) > 0 {
		if raw, err := json.Marshal(props); err == nil {
			paramFile = fmt.Sprintf("%s: %s", velaprocess.ParameterFieldName, string(raw))
		}
	}
	c, err := sourceContext(r.ctx, sourceName)
	if err != nil {
		return policy, err
	}
	// storage: is pure interpolation over context and parameter values, so it is
	// resolved WITHOUT running provider functions. Resolving them here would
	// perform the very I/O the cache exists to avoid - on every reconcile, before
	// the cache is even consulted.
	val, err := velacuex.WorkloadCompiler.Get().CompileStringWithOptions(r.ctx.GetCtx(), strings.Join([]string{
		renderTemplate(sourceTemplate), paramFile, c,
	}, "\n"), upstreamcuex.DisableResolveProviderFunctions{})
	if err != nil {
		return policy, errors.WithMessagef(err, "evaluate storage block for source %q", sourceName)
	}
	storage := val.LookupPath(value.FieldPath("storage"))
	if !storage.Exists() {
		return policy, fmt.Errorf("source definition %q must declare a storage: block with a key: field", sourceType)
	}

	cacheKey := ""
	if err := storage.LookupPath(value.FieldPath("key")).Decode(&cacheKey); err != nil {
		return policy, errors.WithMessagef(err, "resolve storage.key for source %q", sourceName)
	}
	if err := cachekey.ValidateCacheKey(cacheKey); err != nil {
		return policy, errors.WithMessagef(err, "source %q", sourceName)
	}
	policy.Key = cacheKey

	ttlRaw := ""
	if err := storage.LookupPath(value.FieldPath("storageTTL")).Decode(&ttlRaw); err == nil && ttlRaw != "" {
		ttl, err := time.ParseDuration(ttlRaw)
		if err != nil {
			return policy, fmt.Errorf("source %q has an invalid storageTTL %q: %w", sourceName, ttlRaw, err)
		}
		if ttl <= 0 {
			return policy, fmt.Errorf("source %q has a non-positive storageTTL %q", sourceName, ttlRaw)
		}
		policy.TTL = ttl
	}

	onStaleFailure := ""
	if err := storage.LookupPath(value.FieldPath("onStaleFailure")).Decode(&onStaleFailure); err == nil && onStaleFailure != "" {
		switch onStaleFailure {
		case sourceCachePolicyUseStale, sourceCachePolicyFail:
			policy.OnStaleFailure = onStaleFailure
		default:
			// Silently defaulting here would downgrade a definition that asked to
			// fail on stale data into one that serves it.
			return policy, fmt.Errorf("source %q has an unknown onStaleFailure %q: expected %q or %q",
				sourceName, onStaleFailure, sourceCachePolicyUseStale, sourceCachePolicyFail)
		}
	}
	return policy, nil
}

func (r *sourceResolver) validateResolvedOutput(sourceType, sourceTemplate string, output map[string]interface{}) error {
	schemaExpr := r.sourceSchemas[sourceType]
	if schemaExpr == "" {
		extracted, err := extractSourceSchemaExpr(sourceTemplate)
		if err != nil {
			return err
		}
		if extracted == "" {
			return nil
		}
		schemaExpr = extracted
		r.sourceSchemas[sourceType] = extracted
	}
	raw, err := json.Marshal(output)
	if err != nil {
		return err
	}
	v := cuecontext.New().CompileString(fmt.Sprintf("schema: %s\noutput: close(schema) & %s", schemaExpr, string(raw)))
	if v.Err() != nil {
		return v.Err()
	}
	out := v.LookupPath(cue.ParsePath("output"))
	if !out.Exists() {
		return fmt.Errorf("source output missing")
	}
	return out.Validate(cue.Concrete(true))
}

func extractSourceSchemaExpr(template string) (string, error) {
	file, err := cueparser.ParseFile("-", template, cueparser.ParseComments)
	if err != nil {
		return "", err
	}
	for _, decl := range file.Decls {
		field, ok := decl.(*ast.Field)
		if !ok {
			continue
		}
		name, _, err := ast.LabelName(field.Label)
		if err != nil || name != "schema" {
			continue
		}
		bt, err := cueformat.Node(field.Value)
		if err != nil {
			return "", err
		}
		return string(bt), nil
	}
	return "", nil
}

func (r *sourceResolver) readSourceCache(cacheKey string, ttl time.Duration) (map[string]interface{}, bool, bool, time.Time, error) {
	if r.cacheStore == nil || cacheKey == "" {
		return nil, false, false, time.Time{}, nil
	}
	return r.cacheStore.Read(r.ctx.GetCtx(), cacheKey, ttl)
}

func (r *sourceResolver) writeSourceCache(cacheKey, sourceType string, data map[string]interface{}, ttl time.Duration) error {
	if r.cacheStore == nil || cacheKey == "" {
		return nil
	}
	namespace, _ := r.ctx.GetData(velaprocess.ContextNamespace).(string)
	meta := velaprocess.SourceCacheWriteMeta{
		TTL:                ttl,
		SourceDefName:      sourceType,
		SourceDefNamespace: namespace,
		TemplateName:       sourceCacheTemplateName(sourceType, r.sourceSchemas[sourceType]),
	}
	return r.cacheStore.Write(r.ctx.GetCtx(), cacheKey, sourceType, data, meta)
}

// touchSourceCache advances the last-accessed marker for a stale entry that is
// being served, if the backing store supports it. Failures are non-fatal: a
// missed touch only risks the sweep collecting a still-used entry one cycle
// early, which the next render re-creates.
func (r *sourceResolver) touchSourceCache(cacheKey string) {
	if r.cacheStore == nil || cacheKey == "" {
		return
	}
	toucher, ok := r.cacheStore.(velaprocess.SourceCacheToucher)
	if !ok {
		return
	}
	if err := toucher.Touch(r.ctx.GetCtx(), cacheKey); err != nil {
		klog.Warningf("touch source cache failed for %s: %v", cacheKey, err)
	}
}

// sourceCacheTemplateName reproduces the ConfigTemplate name the SourceDefinition
// controller derives from (sourceType, schema) so a cache entry can be stamped
// with the template it was rendered against without a client round-trip. It must
// stay in sync with buildSchemaTemplateName in the sourcedefinition controller.
// Returns "" when there is no schema (no template is generated in that case).
func sourceCacheTemplateName(sourceType, schemaExpr string) string {
	if schemaExpr == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(schemaExpr))
	shortHash := hex.EncodeToString(sum[:])[:8]
	safeName := sanitizeSourceName(sourceType)
	if safeName == "" {
		safeName = "source"
	}
	const prefix = "source-"
	suffix := "-" + shortHash
	maxNameLen := 63 - len(prefix) - len(suffix)
	if maxNameLen < 1 {
		maxNameLen = 1
	}
	if len(safeName) > maxNameLen {
		safeName = strings.Trim(safeName[:maxNameLen], "-")
		if safeName == "" {
			safeName = "source"
		}
	}
	return prefix + safeName + suffix
}

func sanitizeSourceName(name string) string {
	s := strings.ToLower(name)
	var b strings.Builder
	b.Grow(len(s))
	lastDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// applySourceCacheMetadata stamps identity and lifetime metadata onto a source
// cache object so a context-free GC sweep can reason about it. It is strictly
// additive: it never overwrites the config.oam.dev/type label, which callers
// (e.g. the config-API store via ParseConfig) set to the ConfigTemplate name and
// which the config factory relies on for its change-template guard. The
// ttl/template/sourcedefinition markers are new.
func ApplySourceCacheMetadata(obj metav1.Object, sourceType string, meta velaprocess.SourceCacheWriteMeta) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[apitypes.LabelConfigCatalog] = apitypes.VelaCoreConfig
	// Preserve an existing type (the template name set by ParseConfig); only fall
	// back to the source type when nothing linked a template (the Secret-store
	// path, which has no ConfigTemplate).
	if labels[apitypes.LabelConfigType] == "" {
		labels[apitypes.LabelConfigType] = sourceType
	}
	if meta.SourceDefName != "" {
		labels[apitypes.LabelSourceDefinitionName] = meta.SourceDefName
	}
	if meta.SourceDefNamespace != "" {
		labels[apitypes.LabelSourceDefinitionNamespace] = meta.SourceDefNamespace
	}
	obj.SetLabels(labels)

	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if meta.TTL > 0 {
		annotations[sourceCacheTTLKey] = meta.TTL.String()
	}
	if meta.TemplateName != "" {
		annotations[sourceCacheTemplateKey] = meta.TemplateName
	}
	obj.SetAnnotations(annotations)
}

// shouldTouchSourceCache throttles last-accessed updates: it returns true only
// when no marker exists yet or the existing one is older than half the entry's
// TTL, so a hot stale entry is not rewritten on every reconcile. The TTL is read
// from the entry's own annotation, defaulting to sourceCacheTTL.
func ShouldTouchSourceCache(annotations map[string]string, now time.Time) bool {
	if annotations == nil {
		return true
	}
	raw := annotations[sourceCacheAccessedKey]
	if raw == "" {
		return true
	}
	last, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return true
	}
	ttl := sourceCacheTTL
	if t := annotations[sourceCacheTTLKey]; t != "" {
		if parsed, perr := time.ParseDuration(t); perr == nil && parsed > 0 {
			ttl = parsed
		}
	}
	return now.Sub(last) >= ttl/2
}

func (r *sourceResolver) setSourceStatus(sourceName, sourceType, phase, message, config, expiresAt string, resolved map[string]interface{}) {
	statuses, _ := r.ctx.GetData(SourceResolutionStatusKey).(map[string]SourceResolutionStatus)
	if statuses == nil {
		statuses = map[string]SourceResolutionStatus{}
	}
	current := statuses[sourceName]
	consumed := current.ConsumedFields
	if consumed == nil {
		consumed = map[string]interface{}{}
	}
	statuses[sourceName] = SourceResolutionStatus{
		Name:           sourceName,
		Type:           sourceType,
		Phase:          phase,
		Message:        message,
		Config:         config,
		ExpiresAt:      expiresAt,
		ResolvedFields: resolved,
		ConsumedFields: consumed,
		SensitivePaths: append([]string{}, r.sensitivePaths[sourceType]...),
	}
	r.ctx.PushData(SourceResolutionStatusKey, statuses)
}

func (r *sourceResolver) recordConsumedValue(sourceName, sourceType, path string, v interface{}) {
	statuses, _ := r.ctx.GetData(SourceResolutionStatusKey).(map[string]SourceResolutionStatus)
	if statuses == nil {
		statuses = map[string]SourceResolutionStatus{}
	}
	st := statuses[sourceName]
	if st.Name == "" {
		st.Name = sourceName
	}
	if st.Type == "" {
		st.Type = sourceType
	}
	if st.ConsumedFields == nil {
		st.ConsumedFields = map[string]interface{}{}
	}
	st.ConsumedFields[path] = v
	if len(st.SensitivePaths) == 0 {
		st.SensitivePaths = append([]string{}, r.sensitivePaths[sourceType]...)
	}
	statuses[sourceName] = st
	r.ctx.PushData(SourceResolutionStatusKey, statuses)
}
