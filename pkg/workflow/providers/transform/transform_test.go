package transform

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	oamprovidertypes "github.com/oam-dev/kubevela/pkg/workflow/providers/types"
)

func TestReshapeBasic(t *testing.T) {
	r := require.New(t)
	res, err := Reshape(context.Background(), &oamprovidertypes.Params[reshapeParams]{
		Params: reshapeParams{
			Input: map[string]any{"a": 1, "b": 2},
			Query: ".a",
		},
	})
	r.NoError(err)
	r.Equal(1, res.Returns.Output)
}

func TestReshapeWithVariables(t *testing.T) {
	r := require.New(t)
	res, err := Reshape(context.Background(), &oamprovidertypes.Params[reshapeParams]{
		Params: reshapeParams{
			Input: map[string]any{"a": 1},
			Query: ".a + $inc",
			Vars:  map[string]any{"inc": 2},
		},
	})
	r.NoError(err)
	r.Equal(3, res.Returns.Output)
}

func TestReshapeStreamReturnsArray(t *testing.T) {
	r := require.New(t)
	res, err := Reshape(context.Background(), &oamprovidertypes.Params[reshapeParams]{
		Params: reshapeParams{
			Input: []any{1, 2, 3},
			Query: ".[]",
		},
	})
	r.NoError(err)
	r.Equal([]any{1, 2, 3}, res.Returns.Output)
}

func TestReshapeInvalidQuery(t *testing.T) {
	r := require.New(t)
	_, err := Reshape(context.Background(), &oamprovidertypes.Params[reshapeParams]{
		Params: reshapeParams{
			Input: map[string]any{"a": 1},
			Query: ".a |",
		},
	})
	r.Error(err)
}
