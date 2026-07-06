package dispatch

import (
	"context"
	_ "embed"
	"fmt"

	"cuelang.org/go/cue"
	cuexruntime "github.com/kubevela/pkg/cue/cuex/runtime"
	"github.com/kubevela/workflow/pkg/cue/model/value"
	workflowerrors "github.com/kubevela/workflow/pkg/errors"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	oamprovidertypes "github.com/oam-dev/kubevela/pkg/workflow/providers/types"
)

type transformParams struct {
	Type      string `json:"type,omitempty"`
	Component struct {
		Name string `json:"name,omitempty"`
	} `json:"component,omitempty"`
	Target struct {
		Cluster   string `json:"cluster,omitempty"`
		Namespace string `json:"namespace,omitempty"`
	} `json:"target,omitempty"`
	Resources struct {
		Output  map[string]any            `json:"output,omitempty"`
		Outputs map[string]map[string]any `json:"outputs,omitempty"`
	} `json:"resources,omitempty"`
}

// PrepareDispatch prepares resources for dispatch based on type.
func PrepareDispatch(ctx context.Context, params *oamprovidertypes.Params[transformParams]) (*oamprovidertypes.Returns[map[string]any], error) {
	p := params.Params
	if p.Type == "" || p.Type == "cluster-gateway" {
		return &oamprovidertypes.Returns[map[string]any]{
			Returns: map[string]any{
				"output":  p.Resources.Output,
				"outputs": p.Resources.Outputs,
			},
		}, nil
	}
	if p.Type != "cluster-gateway" {
		return nil, fmt.Errorf("unsupported dispatcher type %q", p.Type)
	}
	return &oamprovidertypes.Returns[map[string]any]{Returns: map[string]any{"output": p.Resources.Output, "outputs": p.Resources.Outputs}}, nil
}

// PrepareDispatchNative enables direct CUE invocation for nested value payloads.
func PrepareDispatchNative(ctx context.Context, params *oamprovidertypes.Params[cue.Value]) (cue.Value, error) {
	v := params.Params
	parameter := v.LookupPath(cue.ParsePath("$params"))
	if !parameter.Exists() {
		return cue.Value{}, workflowerrors.LookUpNotFoundErr("$params")
	}
	var p transformParams
	if err := value.UnmarshalTo(parameter, &p); err != nil {
		return cue.Value{}, err
	}
	ret, err := PrepareDispatch(ctx, &oamprovidertypes.Params[transformParams]{Params: p, RuntimeParams: params.RuntimeParams})
	if err != nil {
		return cue.Value{}, err
	}
	return v.FillPath(value.FieldPath("$returns"), ret.Returns), nil
}

// Transform is a backward-compat alias for PrepareDispatch.
func Transform(ctx context.Context, params *oamprovidertypes.Params[transformParams]) (*oamprovidertypes.Returns[map[string]any], error) {
	return PrepareDispatch(ctx, params)
}

// TransformNative is a backward-compat alias for PrepareDispatchNative.
func TransformNative(ctx context.Context, params *oamprovidertypes.Params[cue.Value]) (cue.Value, error) {
	return PrepareDispatchNative(ctx, params)
}

type getPolicyByNameParams struct {
	Name string `json:"name"`
}

type getPolicyByNameReturns struct {
	Policy *v1beta1.AppPolicy `json:"policy,omitempty"`
}

func GetPolicyByName(_ context.Context, params *oamprovidertypes.Params[getPolicyByNameParams]) (*oamprovidertypes.Returns[getPolicyByNameReturns], error) {
	for _, p := range params.Appfile.Policies {
		if p.Name == params.Params.Name {
			cp := p.DeepCopy()
			return &oamprovidertypes.Returns[getPolicyByNameReturns]{Returns: getPolicyByNameReturns{Policy: cp}}, nil
		}
	}
	return &oamprovidertypes.Returns[getPolicyByNameReturns]{Returns: getPolicyByNameReturns{}}, nil
}

type getPoliciesByTypeParams struct {
	Type string `json:"type"`
}

type getPoliciesByTypeReturns struct {
	Policies []v1beta1.AppPolicy `json:"policies,omitempty"`
}

func GetPoliciesByType(_ context.Context, params *oamprovidertypes.Params[getPoliciesByTypeParams]) (*oamprovidertypes.Returns[getPoliciesByTypeReturns], error) {
	matched := make([]v1beta1.AppPolicy, 0)
	for _, p := range params.Appfile.Policies {
		if p.Type == params.Params.Type {
			matched = append(matched, *p.DeepCopy())
		}
	}
	return &oamprovidertypes.Returns[getPoliciesByTypeReturns]{Returns: getPoliciesByTypeReturns{Policies: matched}}, nil
}

//go:embed dispatch.cue
var template string

func GetTemplate() string { return template }

func GetProviders() map[string]cuexruntime.ProviderFn {
	return map[string]cuexruntime.ProviderFn{
		"prepare-dispatch":     oamprovidertypes.NativeProviderFn(PrepareDispatchNative),
		"transform":            oamprovidertypes.NativeProviderFn(TransformNative),
		"get-policy-by-name":   oamprovidertypes.GenericProviderFn[getPolicyByNameParams, oamprovidertypes.Returns[getPolicyByNameReturns]](GetPolicyByName),
		"get-policies-by-type": oamprovidertypes.GenericProviderFn[getPoliciesByTypeParams, oamprovidertypes.Returns[getPoliciesByTypeReturns]](GetPoliciesByType),
	}
}
