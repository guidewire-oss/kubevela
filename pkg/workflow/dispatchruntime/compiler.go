package dispatchruntime

import (
	"context"

	"github.com/kubevela/pkg/cue/cuex"
	cuexruntime "github.com/kubevela/pkg/cue/cuex/runtime"
	"github.com/kubevela/pkg/util/runtime"
	"github.com/kubevela/pkg/util/singleton"
	"k8s.io/klog/v2"

	pkgmulticluster "github.com/oam-dev/kubevela/pkg/multicluster"
	dispatchprovider "github.com/oam-dev/kubevela/pkg/workflow/providers/dispatch"
	oamprovidertypes "github.com/oam-dev/kubevela/pkg/workflow/providers/types"
	transformprovider "github.com/oam-dev/kubevela/pkg/workflow/providers/transform"
)

// Compiler is a dedicated compiler for dispatcher templates.
// It intentionally exposes a minimal package surface.
var Compiler = singleton.NewSingleton[*cuex.Compiler](func() *cuex.Compiler {
	compiler := cuex.NewCompilerWithInternalPackages(
		runtime.Must(cuexruntime.NewInternalPackage("dispatch", dispatchprovider.GetTemplate(), dispatchprovider.GetProviders())),
		runtime.Must(cuexruntime.NewInternalPackage("multicluster", dispatchMulticlusterTemplate, dispatchMulticlusterProviders())),
		runtime.Must(cuexruntime.NewInternalPackage("transform", transformprovider.GetTemplate(), transformprovider.GetProviders())),
	)
	if cuex.EnableExternalPackageForDefaultCompiler {
		if err := compiler.LoadExternalPackages(context.Background()); err != nil {
			klog.Errorf("failed to load external packages for dispatch compiler: %v", err.Error())
		}
	}
	return compiler
})

type dispatchClusterLabelsParams struct {
	Cluster string `json:"cluster"`
}

type dispatchClusterLabelsReturns struct {
	Labels map[string]string `json:"labels"`
}

const dispatchMulticlusterTemplate = `
#GetClusterLabels: {
	#provider: "multicluster"
	#do:       "get-cluster-labels"

	$params: {
		cluster: string
	}
	$returns?: {
		labels: [string]: string
	}
}
`

func dispatchGetClusterLabels(ctx context.Context, params *oamprovidertypes.Params[dispatchClusterLabelsParams]) (*oamprovidertypes.Returns[dispatchClusterLabelsReturns], error) {
	vc, err := pkgmulticluster.GetVirtualCluster(ctx, params.KubeClient, params.Params.Cluster)
	if err != nil {
		return nil, err
	}
	labels := vc.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return &oamprovidertypes.Returns[dispatchClusterLabelsReturns]{
		Returns: dispatchClusterLabelsReturns{
			Labels: labels,
		},
	}, nil
}

func dispatchMulticlusterProviders() map[string]cuexruntime.ProviderFn {
	return map[string]cuexruntime.ProviderFn{
		"get-cluster-labels": oamprovidertypes.GenericProviderFn[dispatchClusterLabelsParams, oamprovidertypes.Returns[dispatchClusterLabelsReturns]](dispatchGetClusterLabels),
	}
}
