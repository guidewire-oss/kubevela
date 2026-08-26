/*
 Copyright 2026. The KubeVela Authors.

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

package operation

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oamv1alpha1 "github.com/kubevela/pkg/apis/oam/v1alpha1"
	workflowv1alpha1 "github.com/kubevela/workflow/api/v1alpha1"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v2alpha1"
)

func operationTestClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, v2alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v2alpha1.Operation{}).
		Build()
}

func TestOperationSuspend(t *testing.T) {
	ctx := context.Background()

	testCases := map[string]struct {
		op          *v2alpha1.Operation
		step        string
		expected    workflowv1alpha1.WorkflowRunStatus
		expectedErr string
	}{
		"terminated": {
			op: &v2alpha1.Operation{
				ObjectMeta: metav1.ObjectMeta{Name: "terminated"},
				Status:     v2alpha1.OperationStatus{Phase: v2alpha1.OperationPhaseSucceeded},
			},
			expectedErr: "can not suspend a terminated operation",
		},
		"suspend all": {
			op: &v2alpha1.Operation{
				ObjectMeta: metav1.ObjectMeta{Name: "suspend-all"},
				Status: v2alpha1.OperationStatus{
					Phase: v2alpha1.OperationPhaseRunning,
					Workflows: []v2alpha1.OperationWorkflowStatus{{
						WorkflowRunStatus: workflowv1alpha1.WorkflowRunStatus{
							Steps: []workflowv1alpha1.WorkflowStepStatus{
								{
									StepStatus: workflowv1alpha1.StepStatus{Name: "step1", Phase: workflowv1alpha1.WorkflowStepPhaseRunning},
									SubStepsStatus: []workflowv1alpha1.StepStatus{
										{Name: "sub1", Phase: workflowv1alpha1.WorkflowStepPhaseRunning},
									},
								},
								{
									StepStatus: workflowv1alpha1.StepStatus{Name: "step2", Phase: workflowv1alpha1.WorkflowStepPhaseSucceeded},
								},
							},
						},
					}},
				},
			},
			expected: workflowv1alpha1.WorkflowRunStatus{
				Suspend: true,
				Steps: []workflowv1alpha1.WorkflowStepStatus{
					{
						StepStatus: workflowv1alpha1.StepStatus{Name: "step1", Phase: workflowv1alpha1.WorkflowStepPhaseSuspending},
						SubStepsStatus: []workflowv1alpha1.StepStatus{
							{Name: "sub1", Phase: workflowv1alpha1.WorkflowStepPhaseSuspending},
						},
					},
					{
						StepStatus: workflowv1alpha1.StepStatus{Name: "step2", Phase: workflowv1alpha1.WorkflowStepPhaseSucceeded},
					},
				},
			},
		},
		"suspend specific step": {
			step: "step1",
			op: &v2alpha1.Operation{
				ObjectMeta: metav1.ObjectMeta{Name: "suspend-step"},
				Status: v2alpha1.OperationStatus{
					Phase: v2alpha1.OperationPhaseRunning,
					Workflows: []v2alpha1.OperationWorkflowStatus{{
						WorkflowRunStatus: workflowv1alpha1.WorkflowRunStatus{
							Steps: []workflowv1alpha1.WorkflowStepStatus{
								{StepStatus: workflowv1alpha1.StepStatus{Name: "step1", Phase: workflowv1alpha1.WorkflowStepPhaseRunning}},
								{StepStatus: workflowv1alpha1.StepStatus{Name: "step2", Phase: workflowv1alpha1.WorkflowStepPhaseRunning}},
							},
						},
					}},
				},
			},
			expected: workflowv1alpha1.WorkflowRunStatus{
				Suspend: true,
				Steps: []workflowv1alpha1.WorkflowStepStatus{
					{StepStatus: workflowv1alpha1.StepStatus{Name: "step1", Phase: workflowv1alpha1.WorkflowStepPhaseSuspending}},
					{StepStatus: workflowv1alpha1.StepStatus{Name: "step2", Phase: workflowv1alpha1.WorkflowStepPhaseRunning}},
				},
			},
		},
		"step not found": {
			step: "missing",
			op: &v2alpha1.Operation{
				ObjectMeta: metav1.ObjectMeta{Name: "step-not-found"},
				Status: v2alpha1.OperationStatus{
					Phase:     v2alpha1.OperationPhaseRunning,
					Workflows: []v2alpha1.OperationWorkflowStatus{{}},
				},
			},
			expectedErr: "can not find step missing",
		},
		"no workflow started yet": {
			op: &v2alpha1.Operation{
				ObjectMeta: metav1.ObjectMeta{Name: "not-started"},
				Status:     v2alpha1.OperationStatus{Phase: v2alpha1.OperationPhasePending},
			},
			expectedErr: "has not started a workflow yet",
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			cli := operationTestClient(t)
			r.NoError(cli.Create(ctx, tc.op))

			var err error
			if tc.step == "" {
				err = NewOperationWorkflowOperator(cli, nil, tc.op).Suspend(ctx)
			} else {
				err = NewOperationWorkflowStepOperator(cli, nil, tc.op).Suspend(ctx, tc.step)
			}
			if tc.expectedErr != "" {
				r.Error(err)
				r.Contains(err.Error(), tc.expectedErr)
				return
			}
			r.NoError(err)

			got := &v2alpha1.Operation{}
			r.NoError(cli.Get(ctx, client.ObjectKeyFromObject(tc.op), got))
			r.Equal(tc.expected, got.Status.Workflows[0].WorkflowRunStatus)
		})
	}
}

func TestOperationResume(t *testing.T) {
	ctx := context.Background()

	testCases := map[string]struct {
		op          *v2alpha1.Operation
		step        string
		expected    workflowv1alpha1.WorkflowRunStatus
		expectedErr string
	}{
		"not suspended": {
			op: &v2alpha1.Operation{
				ObjectMeta: metav1.ObjectMeta{Name: "not-suspended"},
				Status: v2alpha1.OperationStatus{
					Phase:     v2alpha1.OperationPhaseRunning,
					Workflows: []v2alpha1.OperationWorkflowStatus{{}},
				},
			},
			expected: workflowv1alpha1.WorkflowRunStatus{},
		},
		"resume all": {
			op: &v2alpha1.Operation{
				ObjectMeta: metav1.ObjectMeta{Name: "resume-all"},
				Status: v2alpha1.OperationStatus{
					Phase: v2alpha1.OperationPhaseSuspended,
					Workflows: []v2alpha1.OperationWorkflowStatus{{
						WorkflowRunStatus: workflowv1alpha1.WorkflowRunStatus{
							Suspend: true,
							Steps: []workflowv1alpha1.WorkflowStepStatus{
								{StepStatus: workflowv1alpha1.StepStatus{Name: "step1", Phase: workflowv1alpha1.WorkflowStepPhaseSuspending}},
							},
						},
					}},
				},
			},
			expected: workflowv1alpha1.WorkflowRunStatus{
				Steps: []workflowv1alpha1.WorkflowStepStatus{
					{StepStatus: workflowv1alpha1.StepStatus{Name: "step1", Phase: workflowv1alpha1.WorkflowStepPhaseRunning}},
				},
			},
		},
		"resume specific step": {
			step: "step1",
			op: &v2alpha1.Operation{
				ObjectMeta: metav1.ObjectMeta{Name: "resume-step"},
				Status: v2alpha1.OperationStatus{
					Phase: v2alpha1.OperationPhaseSuspended,
					Workflows: []v2alpha1.OperationWorkflowStatus{{
						WorkflowRunStatus: workflowv1alpha1.WorkflowRunStatus{
							Suspend: true,
							Steps: []workflowv1alpha1.WorkflowStepStatus{
								{StepStatus: workflowv1alpha1.StepStatus{Name: "step1", Phase: workflowv1alpha1.WorkflowStepPhaseSuspending}},
								{StepStatus: workflowv1alpha1.StepStatus{Name: "step2", Phase: workflowv1alpha1.WorkflowStepPhaseSuspending}},
							},
						},
					}},
				},
			},
			expected: workflowv1alpha1.WorkflowRunStatus{
				Steps: []workflowv1alpha1.WorkflowStepStatus{
					{StepStatus: workflowv1alpha1.StepStatus{Name: "step1", Phase: workflowv1alpha1.WorkflowStepPhaseRunning}},
					{StepStatus: workflowv1alpha1.StepStatus{Name: "step2", Phase: workflowv1alpha1.WorkflowStepPhaseSuspending}},
				},
			},
		},
		"step not found": {
			step: "missing",
			op: &v2alpha1.Operation{
				ObjectMeta: metav1.ObjectMeta{Name: "resume-not-found"},
				Status: v2alpha1.OperationStatus{
					Phase: v2alpha1.OperationPhaseSuspended,
					Workflows: []v2alpha1.OperationWorkflowStatus{{
						WorkflowRunStatus: workflowv1alpha1.WorkflowRunStatus{Suspend: true},
					}},
				},
			},
			expectedErr: "can not find step missing",
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			cli := operationTestClient(t)
			r.NoError(cli.Create(ctx, tc.op))

			var err error
			if tc.step == "" {
				err = NewOperationWorkflowOperator(cli, nil, tc.op).Resume(ctx)
			} else {
				err = NewOperationWorkflowStepOperator(cli, nil, tc.op).Resume(ctx, tc.step)
			}
			if tc.expectedErr != "" {
				r.Error(err)
				r.Contains(err.Error(), tc.expectedErr)
				return
			}
			r.NoError(err)

			got := &v2alpha1.Operation{}
			r.NoError(cli.Get(ctx, client.ObjectKeyFromObject(tc.op), got))
			r.False(got.Status.Workflows[0].Suspend)
			r.Equal(tc.expected, got.Status.Workflows[0].WorkflowRunStatus)
		})
	}
}

func TestOperationRollback(t *testing.T) {
	err := NewOperationWorkflowOperator(nil, nil, nil).Rollback(context.Background())
	require.EqualError(t, err, "cannot rollback an Operation")
}

func TestOperationTerminate(t *testing.T) {
	ctx := context.Background()
	r := require.New(t)
	cli := operationTestClient(t)
	op := &v2alpha1.Operation{
		ObjectMeta: metav1.ObjectMeta{Name: "terminate-me"},
		Status: v2alpha1.OperationStatus{
			Phase: v2alpha1.OperationPhaseRunning,
			Workflows: []v2alpha1.OperationWorkflowStatus{{
				WorkflowRunStatus: workflowv1alpha1.WorkflowRunStatus{Suspend: true},
			}},
		},
	}
	r.NoError(cli.Create(ctx, op))

	r.NoError(NewOperationWorkflowOperator(cli, nil, op).Terminate(ctx))

	got := &v2alpha1.Operation{}
	r.NoError(cli.Get(ctx, client.ObjectKeyFromObject(op), got))
	r.Equal(v2alpha1.OperationPhaseFailed, got.Status.Phase)
	r.True(got.Status.Workflows[0].Terminated)
	r.False(got.Status.Workflows[0].Suspend)
}

func TestOperationRestartGuards(t *testing.T) {
	ctx := context.Background()

	t.Run("cannot restart a running operation", func(t *testing.T) {
		r := require.New(t)
		cli := operationTestClient(t)
		op := &v2alpha1.Operation{
			ObjectMeta: metav1.ObjectMeta{Name: "running"},
			Status:     v2alpha1.OperationStatus{Phase: v2alpha1.OperationPhaseRunning},
		}
		r.NoError(cli.Create(ctx, op))
		err := NewOperationWorkflowOperator(cli, nil, op).Restart(ctx)
		r.ErrorContains(err, "cannot restart a running operation")
	})

	t.Run("no workflow started yet", func(t *testing.T) {
		r := require.New(t)
		cli := operationTestClient(t)
		op := &v2alpha1.Operation{
			ObjectMeta: metav1.ObjectMeta{Name: "pending"},
			Status:     v2alpha1.OperationStatus{Phase: v2alpha1.OperationPhasePending},
		}
		r.NoError(cli.Create(ctx, op))
		err := NewOperationWorkflowOperator(cli, nil, op).Restart(ctx)
		r.ErrorContains(err, "has not started a workflow yet")
	})
}

func TestOperationRestartWholeWorkflow(t *testing.T) {
	ctx := context.Background()
	r := require.New(t)
	cli := operationTestClient(t)

	op := &v2alpha1.Operation{
		ObjectMeta: metav1.ObjectMeta{Name: "whole-restart", Namespace: "default"},
		Status: v2alpha1.OperationStatus{
			Phase:          v2alpha1.OperationPhaseFailed,
			Message:        "boom",
			Attempts:       1,
			Template:       &v2alpha1.OperationTemplateSpec{},
			CompletionTime: &metav1.Time{},
			Workflows: []v2alpha1.OperationWorkflowStatus{{
				Cluster: "local",
				WorkflowRunStatus: workflowv1alpha1.WorkflowRunStatus{
					Terminated:     true,
					Finished:       true,
					ContextBackend: &corev1.ObjectReference{Name: "whole-restart-context", Namespace: "default"},
					Steps: []workflowv1alpha1.WorkflowStepStatus{
						{StepStatus: workflowv1alpha1.StepStatus{Name: "step1", Phase: workflowv1alpha1.WorkflowStepPhaseSucceeded, Message: "ok"}},
						{StepStatus: workflowv1alpha1.StepStatus{Name: "step2", Phase: workflowv1alpha1.WorkflowStepPhaseFailed, Message: "failed hard"}},
					},
				},
			}},
		},
	}
	r.NoError(cli.Create(ctx, op))
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "whole-restart-context", Namespace: "default"},
		Data:       map[string]string{"vars": "{}"},
	}
	r.NoError(cli.Create(ctx, cm))

	r.NoError(NewOperationWorkflowOperator(cli, nil, op).Restart(ctx))

	got := &v2alpha1.Operation{}
	r.NoError(cli.Get(ctx, client.ObjectKeyFromObject(op), got))
	r.Equal(v2alpha1.OperationPhaseRunning, got.Status.Phase)
	r.Nil(got.Status.CompletionTime)
	r.EqualValues(2, got.Status.Attempts)
	r.Equal(workflowv1alpha1.WorkflowRunStatus{}, got.Status.Workflows[0].WorkflowRunStatus)

	// Prior attempts are preserved even though the live Steps were wiped.
	r.Equal([]v2alpha1.OperationStepAttempt{{AttemptNumber: 1, Phase: workflowv1alpha1.WorkflowStepPhaseSucceeded, Message: "ok"}},
		got.Status.Workflows[0].StepAttempts["step1"])
	r.Equal([]v2alpha1.OperationStepAttempt{{AttemptNumber: 1, Phase: workflowv1alpha1.WorkflowStepPhaseFailed, Message: "failed hard"}},
		got.Status.Workflows[0].StepAttempts["step2"])

	// The old context-backend ConfigMap is deleted, not just orphaned.
	err := cli.Get(ctx, client.ObjectKeyFromObject(cm), &corev1.ConfigMap{})
	r.True(kerrors.IsNotFound(err))
}

func TestOperationRestartFromStep(t *testing.T) {
	ctx := context.Background()

	steps := []oamv1alpha1.WorkflowStep{
		{WorkflowStepBase: oamv1alpha1.WorkflowStepBase{Name: "step1", Outputs: oamv1alpha1.StepOutputs{{Name: "step1-output1"}}}},
		{WorkflowStepBase: oamv1alpha1.WorkflowStepBase{Name: "step2", Outputs: oamv1alpha1.StepOutputs{{Name: "step2-output1"}}}},
		{WorkflowStepBase: oamv1alpha1.WorkflowStepBase{Name: "step3", Outputs: oamv1alpha1.StepOutputs{{Name: "step3-output1"}}}},
	}
	vars := map[string]any{
		"step1-output1": "step1-output1",
		"step2-output1": "step2-output1",
		"step3-output1": "step3-output1",
	}

	newOp := func(name string, phase workflowv1alpha1.WorkflowStepPhase) *v2alpha1.Operation {
		return &v2alpha1.Operation{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Status: v2alpha1.OperationStatus{
				Phase:    v2alpha1.OperationPhaseFailed,
				Template: &v2alpha1.OperationTemplateSpec{Workflow: oamv1alpha1.WorkflowSpec{Steps: steps}},
				Workflows: []v2alpha1.OperationWorkflowStatus{{
					WorkflowRunStatus: workflowv1alpha1.WorkflowRunStatus{
						ContextBackend: &corev1.ObjectReference{Name: name + "-context", Namespace: "default"},
						Steps: []workflowv1alpha1.WorkflowStepStatus{
							{StepStatus: workflowv1alpha1.StepStatus{Name: "step1", Phase: workflowv1alpha1.WorkflowStepPhaseSucceeded}},
							{StepStatus: workflowv1alpha1.StepStatus{Name: "step2", Phase: phase}},
							{StepStatus: workflowv1alpha1.StepStatus{Name: "step3", Phase: workflowv1alpha1.WorkflowStepPhaseFailed}},
						},
					},
				}},
			},
		}
	}

	// Deliberate divergence from upstream CleanStatusFromStep (RETRY_PLAN.md
	// design decisions #5 and #7): restarting a Succeeded (or Skipped) step
	// is allowed, not just a Failed one.
	for _, phase := range []workflowv1alpha1.WorkflowStepPhase{
		workflowv1alpha1.WorkflowStepPhaseFailed,
		workflowv1alpha1.WorkflowStepPhaseSucceeded,
		workflowv1alpha1.WorkflowStepPhaseSkipped,
	} {
		t.Run("restarts step regardless of phase "+string(phase), func(t *testing.T) {
			r := require.New(t)
			cli := operationTestClient(t)
			op := newOp("restart-step-"+string(phase), phase)
			r.NoError(cli.Create(ctx, op))

			b, err := json.Marshal(vars)
			r.NoError(err)
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: op.Name + "-context", Namespace: "default"},
				Data:       map[string]string{"vars": string(b)},
			}
			r.NoError(cli.Create(ctx, cm))

			r.NoError(NewOperationWorkflowStepOperator(cli, nil, op).Restart(ctx, "step2"))

			got := &v2alpha1.Operation{}
			r.NoError(cli.Get(ctx, client.ObjectKeyFromObject(op), got))

			// step1 (before the restart point) is untouched; step2 and step3
			// (the target and everything after it, in sequential mode) are gone
			// from the live status -- they'll re-run from scratch.
			r.Equal([]workflowv1alpha1.WorkflowStepStatus{
				{StepStatus: workflowv1alpha1.StepStatus{Name: "step1", Phase: workflowv1alpha1.WorkflowStepPhaseSucceeded}},
			}, got.Status.Workflows[0].Steps)

			r.Len(got.Status.Workflows[0].StepAttempts["step2"], 1)
			r.Equal(phase, got.Status.Workflows[0].StepAttempts["step2"][0].Phase)
			r.Len(got.Status.Workflows[0].StepAttempts["step3"], 1)

			// step1's recorded output survives; step2 and step3's are cleared,
			// so a restarted step2 still sees step1's output via `inputs`.
			gotCM := &corev1.ConfigMap{}
			r.NoError(cli.Get(ctx, client.ObjectKey{Name: op.Name + "-context", Namespace: "default"}, gotCM))
			r.JSONEq(`{"step1-output1":"step1-output1"}`, gotCM.Data["vars"])

			r.EqualValues(1, got.Status.Attempts)
			r.Equal(v2alpha1.OperationPhaseRunning, got.Status.Phase)
		})
	}

	t.Run("step not found", func(t *testing.T) {
		r := require.New(t)
		cli := operationTestClient(t)
		op := newOp("restart-missing-step", workflowv1alpha1.WorkflowStepPhaseFailed)
		r.NoError(cli.Create(ctx, op))
		err := NewOperationWorkflowStepOperator(cli, nil, op).Restart(ctx, "no-such-step")
		r.ErrorContains(err, "no-such-step not found")
	})
}
