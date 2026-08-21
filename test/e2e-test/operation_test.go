/*
Copyright 2026 The KubeVela Authors.

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

package controllers_test

import (
	"context"
	"strconv"
	"time"

	"math/rand"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v2alpha1"
	"github.com/oam-dev/kubevela/pkg/oam/util"
	"github.com/oam-dev/kubevela/pkg/utils/common"
)

// applyRestartWebserviceWorkflowStepDefinition installs the restart-webservice
// WorkflowStepDefinition test fixture into vela-system, the namespace
// WorkflowStepDefinitions resolve from. Torn down in AfterEach, since
// vela-system isn't otherwise reset between runs of this suite.
func applyRestartWebserviceWorkflowStepDefinition(ctx context.Context) {
	var def v1beta1.WorkflowStepDefinition
	Expect(common.ReadYamlToObject("testdata/operation/vela-system/workflowstepdefinition.yaml", &def)).Should(BeNil())
	Expect(k8sClient.Create(ctx, &def)).Should(SatisfyAny(BeNil(), &util.AlreadyExistMatcher{}))
}

// applyOperationTemplate installs the restart-webservice OperationTemplate
// fixture into vela-system, exercising the same two-tier resolution used
// for ComponentDefinition. Torn down in AfterEach.
func applyOperationTemplate(ctx context.Context) {
	var tmpl v2alpha1.OperationTemplate
	Expect(common.ReadYamlToObject("testdata/operation/vela-system/operationtemplate.yaml", &tmpl)).Should(BeNil())
	Expect(k8sClient.Create(ctx, &tmpl)).Should(SatisfyAny(BeNil(), &util.AlreadyExistMatcher{}))
}

// applyOperationRBAC installs the restart Job's identity from the same
// fixtures the manual walkthrough applies. Torn down in AfterEach.
func applyOperationRBAC(ctx context.Context) {
	var sa corev1.ServiceAccount
	Expect(common.ReadYamlToObject("testdata/operation/vela-system/serviceaccount.yaml", &sa)).Should(BeNil())
	Expect(k8sClient.Create(ctx, &sa)).Should(SatisfyAny(BeNil(), &util.AlreadyExistMatcher{}))

	var role rbacv1.ClusterRole
	Expect(common.ReadYamlToObject("testdata/operation/vela-system/clusterrole.yaml", &role)).Should(BeNil())
	Expect(k8sClient.Create(ctx, &role)).Should(SatisfyAny(BeNil(), &util.AlreadyExistMatcher{}))

	var roleBinding rbacv1.ClusterRoleBinding
	Expect(common.ReadYamlToObject("testdata/operation/vela-system/clusterrolebinding.yaml", &roleBinding)).Should(BeNil())
	Expect(k8sClient.Create(ctx, &roleBinding)).Should(SatisfyAny(BeNil(), &util.AlreadyExistMatcher{}))
}

// deleteOperationVelaSystemFixtures removes everything the functions above
// install into vela-system, so the next run never sees a stale Spec left
// over from this one.
func deleteOperationVelaSystemFixtures(ctx context.Context) {
	objs := []client.Object{
		&v1beta1.WorkflowStepDefinition{ObjectMeta: metav1.ObjectMeta{Name: "restart-webservice", Namespace: "vela-system"}},
		&v2alpha1.OperationTemplate{ObjectMeta: metav1.ObjectMeta{Name: "restart-webservice", Namespace: "vela-system"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "restart-webservice", Namespace: "vela-system"}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "restart-webservice"}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "restart-webservice"}},
	}
	for _, obj := range objs {
		Expect(k8sClient.Delete(ctx, obj)).Should(SatisfyAny(BeNil(), &util.NotFoundMatcher{}))
	}
}

// Smoke test for the v2alpha1 Operation controller (KEP 2.15).
var _ = Describe("Operation (v2alpha1)", func() {
	ctx := context.Background()
	var namespaceName string
	var ns corev1.Namespace

	BeforeEach(func() {
		namespaceName = "operation-e2e-test" + "-" + strconv.FormatInt(rand.Int63(), 16)
		ns = createNamespace(ctx, namespaceName)
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &ns, client.PropagationPolicy(metav1.DeletePropagationBackground))).Should(BeNil())
		deleteOperationVelaSystemFixtures(ctx)
	})

	It("restarts the target Deployment, and only reaches phase Succeeded once the Job completes", func() {
		By("applying the restart-webservice WorkflowStepDefinition (vela-system)")
		applyRestartWebserviceWorkflowStepDefinition(ctx)

		By("applying the restart-webservice OperationTemplate (vela-system)")
		applyOperationTemplate(ctx)

		By("applying RBAC for the restart Job's ServiceAccount (vela-system)")
		applyOperationRBAC(ctx)

		By("applying the webservice Application")
		var app v1beta1.Application
		Expect(common.ReadYamlToObject("testdata/operation/app.yaml", &app)).Should(BeNil())
		app.Namespace = namespaceName
		Expect(k8sClient.Create(ctx, &app)).Should(BeNil())

		By("waiting for the webservice component's Deployment to be ready")
		deployment := &appsv1.Deployment{}
		Eventually(func() int32 {
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespaceName, Name: "webservice"}, deployment); err != nil {
				return 0
			}
			return deployment.Status.ReadyReplicas
		}, 2*time.Minute, 2*time.Second).Should(Equal(int32(1)))
		beforeGeneration := deployment.Generation

		By("creating the Operation, resolving its template from vela-system")
		var op v2alpha1.Operation
		Expect(common.ReadYamlToObject("testdata/operation/operation.yaml", &op)).Should(BeNil())
		op.Namespace = namespaceName
		Expect(k8sClient.Create(ctx, &op)).Should(BeNil())

		By("waiting for the Operation to reach a terminal phase")
		Eventually(func() v2alpha1.OperationPhase {
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespaceName, Name: "restart-op"}, &op); err != nil {
				return ""
			}
			return op.Status.Phase
		}, 2*time.Minute, 2*time.Second).Should(Equal(v2alpha1.OperationPhaseSucceeded),
			"the Operation should only reach Succeeded once its workflow's Job step actually completes, not merely once the Job is created")

		By("verifying the Deployment was actually restarted")
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespaceName, Name: "webservice"}, deployment)).Should(BeNil())
		Expect(deployment.Generation).To(BeNumerically(">", beforeGeneration))
		Expect(deployment.Spec.Template.Annotations).To(HaveKey("kubectl.kubernetes.io/restartedAt"))

		By("verifying the Operation's spec.parameters.reason reached the Deployment via context.operationParams")
		Expect(deployment.Annotations).To(HaveKeyWithValue("operation.oam.dev/restart-reason", "e2e-test-restart"))

		By("verifying the step read the Deployment's status via context.output")
		pods := &corev1.PodList{}
		Expect(k8sClient.List(ctx, pods, client.InNamespace("vela-system"),
			client.MatchingLabels{"workflow.oam.dev/step-name": "webservice-restart"})).Should(BeNil())
		Expect(pods.Items).NotTo(BeEmpty())
		for _, pod := range pods.Items {
			Expect(pod.Spec.Containers[0].Env).To(ContainElement(
				corev1.EnvVar{Name: "READY_REPLICAS", Value: "1"}))
		}
	})
})
