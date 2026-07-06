package transform

import (
	"context"
	"encoding/json"
	_ "embed"
	"fmt"
	"sort"

	"cuelang.org/go/cue"
	cuexruntime "github.com/kubevela/pkg/cue/cuex/runtime"
	"github.com/kubevela/workflow/pkg/cue/model/value"
	workflowerrors "github.com/kubevela/workflow/pkg/errors"
	"github.com/itchyny/gojq"
	"k8s.io/klog/v2"

	oamprovidertypes "github.com/oam-dev/kubevela/pkg/workflow/providers/types"
)

type reshapeParams struct {
	Input any            `json:"input,omitempty"`
	Query string         `json:"query,omitempty"`
	Vars  map[string]any `json:"vars,omitempty"`
}

type reshapeReturns struct {
	Output any `json:"output,omitempty"`
}

// Reshape runs a gojq query against arbitrary input data.
func Reshape(ctx context.Context, params *oamprovidertypes.Params[reshapeParams]) (*oamprovidertypes.Returns[reshapeReturns], error) {
	p := params.Params
	if p.Query == "" {
		return nil, fmt.Errorf("transform reshape requires non-empty query")
	}
	q, err := gojq.Parse(p.Query)
	if err != nil {
		klog.Warningf("transform: reshape parse failed err=%v", err)
		return nil, err
	}
	opts, args := buildVariableOptionsAndArgs(p.Vars)
	code, err := gojq.Compile(q, opts...)
	if err != nil {
		klog.Warningf("transform: reshape compile failed err=%v", err)
		return nil, err
	}
	iter := code.RunWithContext(ctx, p.Input, args...)
	out, err := collectIteratorResults(iter)
	if err != nil {
		klog.Warningf("transform: reshape eval failed err=%v", err)
		return nil, err
	}
	klog.Infof("transform: reshape output=%s", marshalForPrettyLog(out))
	return &oamprovidertypes.Returns[reshapeReturns]{
		Returns: reshapeReturns{Output: out},
	}, nil
}

func marshalForPrettyLog(v any) string {
	bs, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(bs)
}

func buildVariableOptionsAndArgs(vars map[string]any) ([]gojq.CompilerOption, []any) {
	if len(vars) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(vars))
	for k := range vars {
		names = append(names, k)
	}
	sort.Strings(names)
	variableNames := make([]string, 0, len(names))
	args := make([]any, 0, len(names))
	for _, name := range names {
		variableNames = append(variableNames, "$"+name)
		args = append(args, vars[name])
	}
	return []gojq.CompilerOption{gojq.WithVariables(variableNames)}, args
}

func collectIteratorResults(iter gojq.Iter) (any, error) {
	results := make([]any, 0, 1)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, ok := v.(error); ok {
			return nil, err
		}
		results = append(results, v)
	}
	switch len(results) {
	case 0:
		return nil, nil
	case 1:
		return results[0], nil
	default:
		return results, nil
	}
}

// ReshapeNative enables direct CUE invocation for nested value payloads.
func ReshapeNative(ctx context.Context, params *oamprovidertypes.Params[cue.Value]) (cue.Value, error) {
	v := params.Params
	parameter := v.LookupPath(cue.ParsePath("$params"))
	if !parameter.Exists() {
		return cue.Value{}, workflowerrors.LookUpNotFoundErr("$params")
	}
	var p reshapeParams
	if err := value.UnmarshalTo(parameter, &p); err != nil {
		return cue.Value{}, err
	}
	ret, err := Reshape(ctx, &oamprovidertypes.Params[reshapeParams]{Params: p, RuntimeParams: params.RuntimeParams})
	if err != nil {
		return cue.Value{}, err
	}
	return v.FillPath(value.FieldPath("$returns"), ret.Returns), nil
}

//go:embed transform.cue
var template string

func GetTemplate() string { return template }

func GetProviders() map[string]cuexruntime.ProviderFn {
	return map[string]cuexruntime.ProviderFn{
		"reshape": oamprovidertypes.NativeProviderFn(ReshapeNative),
	}
}
