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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/appfile"
	cuedefinition "github.com/oam-dev/kubevela/pkg/cue/definition"
	velaprocess "github.com/oam-dev/kubevela/pkg/cue/process"
)

// componentWithSourceStatuses builds the minimal component context the merge
// reads from: a process context carrying the resolver's per-source statuses.
func componentWithSourceStatuses(statuses map[string]cuedefinition.SourceResolutionStatus) *appfile.Component {
	pCtx := velaprocess.NewContext(velaprocess.ContextData{
		Ctx:       context.Background(),
		Namespace: "default",
		Cluster:   "local",
		AppName:   "test-app",
		CompName:  "test-comp",
	})
	pCtx.PushData(cuedefinition.SourceResolutionStatusKey, statuses)
	return &appfile.Component{Name: "test-comp", Ctx: pCtx}
}

func handlerWithSources(sources ...v1beta1.ApplicationSource) *AppHandler {
	return &AppHandler{
		app: &v1beta1.Application{
			Spec: v1beta1.ApplicationSpec{Sources: sources},
		},
	}
}

// The resolver records a phase for every source it processes, but it is only
// useful if it survives the hand-off onto the Application's status. Without
// this, an operator cannot tell a resolved source from a failed one.
func TestMergeSourceResolutionStatusCarriesPhase(t *testing.T) {
	h := handlerWithSources(v1beta1.ApplicationSource{Name: "s", Type: "demo-source"})
	comp := componentWithSourceStatuses(map[string]cuedefinition.SourceResolutionStatus{
		"s": {
			Name:  "s",
			Type:  "demo-source",
			Phase: "Resolved",
		},
	})

	status := common.ApplicationComponentStatus{Name: "test-comp"}
	h.mergeSourceResolutionStatus(comp, &status)

	require.Len(t, status.Sources, 1)
	require.Equal(t, "s", status.Sources[0].Name)
	require.Equal(t, "Resolved", status.Sources[0].Phase,
		"the phase the resolver recorded must reach the Application status")
}

// A failing source is the case an operator most needs to see. It travels the
// same path as a resolved one, so it is asserted separately rather than
// assumed to follow.
func TestMergeSourceResolutionStatusCarriesFailedPhase(t *testing.T) {
	h := handlerWithSources(v1beta1.ApplicationSource{Name: "s", Type: "demo-source"})
	comp := componentWithSourceStatuses(map[string]cuedefinition.SourceResolutionStatus{
		"s": {
			Name:    "s",
			Type:    "demo-source",
			Phase:   "Failed",
			Message: "configmaps \"missing\" not found",
		},
	})

	status := common.ApplicationComponentStatus{Name: "test-comp"}
	h.mergeSourceResolutionStatus(comp, &status)

	require.Len(t, status.Sources, 1)
	require.Equal(t, "Failed", status.Sources[0].Phase)
	require.Equal(t, "configmaps \"missing\" not found", status.Sources[0].Message)
}

// A source the resolver never reached has no recorded status. It must still be
// listed, and must not claim a phase it was never given.
func TestMergeSourceResolutionStatusLeavesUnresolvedSourcePhaseEmpty(t *testing.T) {
	h := handlerWithSources(v1beta1.ApplicationSource{Name: "untouched", Type: "demo-source"})
	comp := componentWithSourceStatuses(map[string]cuedefinition.SourceResolutionStatus{})

	status := common.ApplicationComponentStatus{Name: "test-comp"}
	h.mergeSourceResolutionStatus(comp, &status)

	require.Len(t, status.Sources, 1)
	require.Equal(t, "untouched", status.Sources[0].Name)
	require.Empty(t, status.Sources[0].Phase)
}
