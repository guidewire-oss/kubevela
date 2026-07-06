package dispatch

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/appfile"
	oamprovidertypes "github.com/oam-dev/kubevela/pkg/workflow/providers/types"
)

func TestPrepareDispatchClusterGateway(t *testing.T) {
	r := require.New(t)
	res, err := PrepareDispatch(context.Background(), &oamprovidertypes.Params[transformParams]{
		Params: transformParams{
			Type: "cluster-gateway",
			Resources: struct {
				Output  map[string]any            `json:"output,omitempty"`
				Outputs map[string]map[string]any `json:"outputs,omitempty"`
			}{
				Output: map[string]any{"kind": "Deployment"},
				Outputs: map[string]map[string]any{
					"srv": {"kind": "Service"},
				},
			},
		},
	})
	r.NoError(err)
	r.Equal("Deployment", res.Returns["output"].(map[string]any)["kind"])
}

func TestTransformUnsupportedType(t *testing.T) {
	r := require.New(t)
	params := transformParams{
		Type: "custom",
	}
	_, err := PrepareDispatch(context.Background(), &oamprovidertypes.Params[transformParams]{Params: params})
	r.Error(err)
	r.Contains(err.Error(), "unsupported dispatcher type")
}

func TestGetPolicyByName(t *testing.T) {
	r := require.New(t)
	res, err := GetPolicyByName(context.Background(), &oamprovidertypes.Params[getPolicyByNameParams]{
		Params: getPolicyByNameParams{Name: "topo"},
		RuntimeParams: oamprovidertypes.RuntimeParams{
			Appfile: &appfile.Appfile{
				Policies: []v1beta1.AppPolicy{{Name: "topo", Type: "topology"}},
			},
		},
	})
	r.NoError(err)
	r.NotNil(res.Returns.Policy)
	r.Equal("topology", res.Returns.Policy.Type)
}

func TestGetPoliciesByType(t *testing.T) {
	r := require.New(t)
	res, err := GetPoliciesByType(context.Background(), &oamprovidertypes.Params[getPoliciesByTypeParams]{
		Params: getPoliciesByTypeParams{Type: "topology"},
		RuntimeParams: oamprovidertypes.RuntimeParams{
			Appfile: &appfile.Appfile{
				Policies: []v1beta1.AppPolicy{
					{Name: "a", Type: "topology"},
					{Name: "b", Type: "override"},
					{Name: "c", Type: "topology"},
				},
			},
		},
	})
	r.NoError(err)
	r.Len(res.Returns.Policies, 2)
}
