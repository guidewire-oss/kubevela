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
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oamcomm "github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
)

var _ = Describe("SourceDefinition fromSource e2e", func() {
	ctx := context.Background()

	var namespaceName string
	var ns corev1.Namespace

	BeforeEach(func() {
		namespaceName = randomNamespaceName("source-definition-e2e")
		ns = createNamespace(ctx, namespaceName)
	})

	AfterEach(func() {
		By("Clean up source-definition e2e namespace")
		Expect(k8sClient.Delete(ctx, &ns, client.PropagationPolicy(metav1.DeletePropagationBackground))).Should(BeNil())
	})

	It("resolves fromSource and reconciles updated source properties", func() {
		sourceDef := &v1beta1.SourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "image-source",
				Namespace: namespaceName,
			},
			Spec: v1beta1.SourceDefinitionSpec{
				Schematic: &oamcomm.Schematic{
					CUE: &oamcomm.CUE{
						Template: `
output: {
  // +sensitive
  image: parameter.image
}
parameter: {
  image: string
}
`,
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, sourceDef)).Should(Succeed())

		app := &v1beta1.Application{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "source-image-app",
				Namespace: namespaceName,
			},
			Spec: v1beta1.ApplicationSpec{
				Sources: []v1beta1.ApplicationSource{
					{
						Name: "img",
						Type: "image-source",
						Properties: &runtime.RawExtension{
							Raw: []byte(`{"image":"nginx:1.25.0"}`),
						},
						StatusPolicy: &v1beta1.ApplicationSourceStatusPolicy{
							ExposeConsumedValues: true,
						},
					},
				},
				Components: []oamcomm.ApplicationComponent{
					{
						Name: "web",
						Type: "webservice",
						Properties: &runtime.RawExtension{
							Raw: []byte(`{
  "image":{"fromSource":"img.image"},
  "port":80
}`),
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, app)).Should(Succeed())

		verifyApplicationPhase(ctx, namespaceName, app.Name, oamcomm.ApplicationRunning)
		Eventually(func() (string, error) {
			deploy := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespaceName, Name: "web"}, deploy); err != nil {
				return "", err
			}
			if len(deploy.Spec.Template.Spec.Containers) == 0 {
				return "", fmt.Errorf("deployment has no containers")
			}
			return deploy.Spec.Template.Spec.Containers[0].Image, nil
		}, 90*time.Second, time.Second).Should(Equal("nginx:1.25.0"))

		Eventually(func() error {
			latest := &v1beta1.Application{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(app), latest); err != nil {
				return err
			}
			latest.Spec.Sources[0].Properties = &runtime.RawExtension{
				Raw: []byte(`{"image":"nginx:1.25.1"}`),
			}
			return k8sClient.Update(ctx, latest)
		}, 20*time.Second, time.Second).Should(Succeed())

		Eventually(func() (string, error) {
			deploy := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespaceName, Name: "web"}, deploy); err != nil {
				return "", err
			}
			if len(deploy.Spec.Template.Spec.Containers) == 0 {
				return "", fmt.Errorf("deployment has no containers")
			}
			return deploy.Spec.Template.Spec.Containers[0].Image, nil
		}, 90*time.Second, time.Second).Should(Equal("nginx:1.25.1"))

		Eventually(func() (string, error) {
			latest := &v1beta1.Application{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(app), latest); err != nil {
				return "", err
			}
			if len(latest.Status.Services) == 0 || len(latest.Status.Services[0].Sources) == 0 {
				return "", fmt.Errorf("source status not ready")
			}
			for _, src := range latest.Status.Services[0].Sources {
				if src.Name != "img" {
					continue
				}
				if src.Properties == nil {
					return "", fmt.Errorf("source properties missing")
				}
				var props map[string]interface{}
				if err := json.Unmarshal(src.Properties.Raw, &props); err != nil {
					return "", err
				}
				image, _ := props["image"].(string)
				if image == "" {
					return "", fmt.Errorf("image property missing")
				}
				return image, nil
			}
			return "", fmt.Errorf("source status for img not found")
		}, 60*time.Second, time.Second).Should(Equal("***"))
	})

	It("resolves chained nested sources where second source depends on first", func() {
		sourceA := &v1beta1.SourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cluster-source",
				Namespace: namespaceName,
			},
			Spec: v1beta1.SourceDefinitionSpec{
				Schematic: &oamcomm.Schematic{
					CUE: &oamcomm.CUE{
						Template: `
output: {
  nested: {
    image: {
      repo: parameter.repo
      tag:  parameter.tag
    }
    meta: {
      region: parameter.region
    }
  }
}
parameter: {
  repo:   string
  tag:    string
  region: string
}
`,
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, sourceA)).Should(Succeed())

		sourceB := &v1beta1.SourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "render-source",
				Namespace: namespaceName,
			},
			Spec: v1beta1.SourceDefinitionSpec{
				Schematic: &oamcomm.Schematic{
					CUE: &oamcomm.CUE{
						Template: `
output: {
  resolved: {
    image: "\(parameter.repo):\(parameter.tag)"
    region: parameter.region
  }
}
parameter: {
  repo:   string
  tag:    string
  region: string
}
`,
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, sourceB)).Should(Succeed())

		app := &v1beta1.Application{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "source-chain-app",
				Namespace: namespaceName,
			},
			Spec: v1beta1.ApplicationSpec{
				Sources: []v1beta1.ApplicationSource{
					{
						Name: "clusterInfo",
						Type: "cluster-source",
						Properties: &runtime.RawExtension{
							Raw: []byte(`{"repo":"nginx","tag":"1.25.2","region":"us-east-1"}`),
						},
					},
					{
						Name: "rendered",
						Type: "render-source",
						Properties: &runtime.RawExtension{
							Raw: []byte(`{
  "repo":{"fromSource":"clusterInfo.nested.image.repo"},
  "tag":{"fromSource":"clusterInfo.nested.image.tag"},
  "region":{"fromSource":"clusterInfo.nested.meta.region"}
}`),
						},
					},
				},
				Components: []oamcomm.ApplicationComponent{
					{
						Name: "web-chain",
						Type: "webservice",
						Properties: &runtime.RawExtension{
							Raw: []byte(`{
  "image":{"fromSource":"rendered.resolved.image"},
  "port":80
}`),
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, app)).Should(Succeed())

		verifyApplicationPhase(ctx, namespaceName, app.Name, oamcomm.ApplicationRunning)
		Eventually(func() (string, error) {
			deploy := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespaceName, Name: "web-chain"}, deploy); err != nil {
				return "", err
			}
			if len(deploy.Spec.Template.Spec.Containers) == 0 {
				return "", fmt.Errorf("deployment has no containers")
			}
			return deploy.Spec.Template.Spec.Containers[0].Image, nil
		}, 90*time.Second, time.Second).Should(Equal("nginx:1.25.2"))
	})

	It("resolves fromSource values in trait properties", func() {
		sourceDef := &v1beta1.SourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "scale-source",
				Namespace: namespaceName,
			},
			Spec: v1beta1.SourceDefinitionSpec{
				Schematic: &oamcomm.Schematic{
					CUE: &oamcomm.CUE{
						Template: `
output: {
  scale: {
    replicas: parameter.replicas
  }
}
parameter: {
  replicas: int
}
`,
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, sourceDef)).Should(Succeed())

		app := &v1beta1.Application{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "source-trait-app",
				Namespace: namespaceName,
			},
			Spec: v1beta1.ApplicationSpec{
				Sources: []v1beta1.ApplicationSource{
					{
						Name: "scaleData",
						Type: "scale-source",
						Properties: &runtime.RawExtension{
							Raw: []byte(`{"replicas":3}`),
						},
					},
				},
				Components: []oamcomm.ApplicationComponent{
					{
						Name: "web-trait",
						Type: "webservice",
						Properties: &runtime.RawExtension{
							Raw: []byte(`{
  "image":"nginx:1.25.0",
  "port":80
}`),
						},
						Traits: []oamcomm.ApplicationTrait{
							{
								Type: "scaler",
								Properties: &runtime.RawExtension{
									Raw: []byte(`{
  "replicas":{"fromSource":"scaleData.scale.replicas"}
}`),
								},
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, app)).Should(Succeed())

		verifyApplicationPhase(ctx, namespaceName, app.Name, oamcomm.ApplicationRunning)
		Eventually(func() (int32, error) {
			deploy := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespaceName, Name: "web-trait"}, deploy); err != nil {
				return 0, err
			}
			if deploy.Spec.Replicas == nil {
				return 0, fmt.Errorf("deployment replicas is nil")
			}
			return *deploy.Spec.Replicas, nil
		}, 90*time.Second, time.Second).Should(Equal(int32(3)))
	})
})
