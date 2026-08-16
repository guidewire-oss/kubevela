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

package spokecluster

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
)

// These tests are plain unit tests (no apiserver): Validate and Default are
// pure functions, and the handlers only need a decoder to turn raw admission
// requests into typed SpokeClusters.

func TestSpokeClusterWebhook(t *testing.T) {
	gomega.RegisterFailHandler(Fail)
	RunSpecs(t, "SpokeCluster Webhook Suite")
}

// decoder and testScheme are shared by the validating and mutating handler specs.
var (
	decoder    admission.Decoder
	testScheme *runtime.Scheme
)

var _ = BeforeSuite(func() {
	testScheme = runtime.NewScheme()
	gomega.Expect(k8sscheme.AddToScheme(testScheme)).To(gomega.Succeed())
	gomega.Expect(v1beta1.AddToScheme(testScheme)).To(gomega.Succeed())
	decoder = admission.NewDecoder(testScheme)
})
