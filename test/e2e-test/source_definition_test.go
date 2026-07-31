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
	"strings"
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
		// Delete the test namespace FIRST so its Applications and SourceDefinitions
		// are gone before we clean vela-system. Otherwise a still-running
		// Application would re-write its source cache entry on the next reconcile,
		// racing (and defeating) the vela-system cleanup below.
		By("Clean up source-definition e2e namespace")
		Expect(k8sClient.Delete(ctx, &ns, client.PropagationPolicy(metav1.DeletePropagationBackground))).Should(BeNil())

		// SourceDefinitions live in the test namespace, but the ConfigTemplates
		// (config-template-source-* ConfigMaps) and source cache entries
		// (source-cache-* Secrets) they generate live in vela-system and are only
		// reclaimed asynchronously by the periodic GC sweep. Delete everything
		// stamped with this test's namespace via the owning-SourceDefinition label
		// so runs do not leak into vela-system. Retry: the label-carrying objects
		// may still be settling as the namespace tears down.
		By("Clean up ConfigTemplates and source cache entries created in vela-system")
		nsLabel := client.MatchingLabels{"sourcedefinition.oam.dev/namespace": namespaceName}
		Eventually(func() error {
			if err := k8sClient.DeleteAllOf(ctx, &corev1.ConfigMap{},
				client.InNamespace("vela-system"), nsLabel); err != nil {
				return err
			}
			if err := k8sClient.DeleteAllOf(ctx, &corev1.Secret{},
				client.InNamespace("vela-system"), nsLabel); err != nil {
				return err
			}
			// Confirm nothing labelled for this namespace remains.
			var cms corev1.ConfigMapList
			if err := k8sClient.List(ctx, &cms, client.InNamespace("vela-system"), nsLabel); err != nil {
				return err
			}
			var secrets corev1.SecretList
			if err := k8sClient.List(ctx, &secrets, client.InNamespace("vela-system"), nsLabel); err != nil {
				return err
			}
			if len(cms.Items)+len(secrets.Items) > 0 {
				return fmt.Errorf("still %d configmaps and %d secrets labelled for %s in vela-system",
					len(cms.Items), len(secrets.Items), namespaceName)
			}
			return nil
		}, 30*time.Second, time.Second).Should(Succeed())
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
schema: {
  image: string
}
storage: {
  // Generated: this source reads no context, so the key is bare. The image is a
  // property, and properties are hashed into the cache identity by the resolver -
  // which is why a value containing ':' and '.' needs no normalising here.
  key: "image-source"
}
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

	It("creates source cache using storage key policy", func() {
		sourceDef := &v1beta1.SourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "stale-image-source",
				Namespace: namespaceName,
			},
			Spec: v1beta1.SourceDefinitionSpec{
				Schematic: &oamcomm.Schematic{
					CUE: &oamcomm.CUE{
						Template: `
schema: {
  image: string
}
storage: {
  // Generated: the template reads no context, so the key is the definition name.
  // storage.key is not itself scanned, so a key mentioning context.namespace
  // would not make namespace a dimension.
  key: "stale-image-source"
  storageTTL: "1h"
  onStaleFailure: "use-stale"
}
output: {
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
		// SourceDefinition controller should publish deterministic ConfigTemplate reference.
		Eventually(func() error {
			latest := &v1beta1.SourceDefinition{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespaceName, Name: "stale-image-source"}, latest); err != nil {
				return err
			}
			if latest.Status.ConfigTemplateRef == nil || latest.Status.ConfigTemplateRef.Name == "" {
				return fmt.Errorf("configTemplateRef not ready")
			}
			if latest.Status.ConfigTemplateRef.SchemaHash == "" {
				return fmt.Errorf("configTemplateRef.schemaHash not ready")
			}
			cm := &corev1.ConfigMap{}
			if err := k8sClient.Get(ctx, client.ObjectKey{
				Namespace: "vela-system",
				Name:      "config-template-" + latest.Status.ConfigTemplateRef.Name,
			}, cm); err != nil {
				return err
			}
			if cm.Labels["config.oam.dev/catalog"] != "velacore-config" {
				return fmt.Errorf("unexpected config catalog label: %q", cm.Labels["config.oam.dev/catalog"])
			}
			if cm.Data["schema"] == "" {
				return fmt.Errorf("missing schema in config template")
			}
			return nil
		}, 60*time.Second, time.Second).Should(Succeed())

		app := &v1beta1.Application{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "source-stale-app",
				Namespace: namespaceName,
			},
			Spec: v1beta1.ApplicationSpec{
				Sources: []v1beta1.ApplicationSource{
					{
						Name: "img",
						Type: "stale-image-source",
						Properties: &runtime.RawExtension{
							Raw: []byte(`{"image":"nginx:1.25.0"}`),
						},
					},
				},
				Components: []oamcomm.ApplicationComponent{
					{
						Name: "web-stale",
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

		// The cache entry is named <storage.key>-<propertiesHash>: the generated key
		// leads so it stays greppable, and the hash discriminates bindings that
		// differ only in their properties. The hash is not knowable here, so match
		// on the prefix rather than pinning a name.
		Eventually(func() error {
			var secrets corev1.SecretList
			if err := k8sClient.List(ctx, &secrets, client.InNamespace("vela-system")); err != nil {
				return err
			}
			var secret *corev1.Secret
			for i := range secrets.Items {
				if strings.HasPrefix(secrets.Items[i].Name, "stale-image-source-") {
					secret = &secrets.Items[i]
					break
				}
			}
			if secret == nil {
				return fmt.Errorf("no source cache entry with prefix %q in vela-system", "stale-image-source-")
			}
			sourceDef := &v1beta1.SourceDefinition{}
			if err := k8sClient.Get(ctx, client.ObjectKey{
				Namespace: namespaceName,
				Name:      "stale-image-source",
			}, sourceDef); err != nil {
				return err
			}
			if secret.Labels["config.oam.dev/catalog"] != "velacore-config" {
				return fmt.Errorf("unexpected config catalog label: %q", secret.Labels["config.oam.dev/catalog"])
			}
			if sourceDef.Status.ConfigTemplateRef == nil || sourceDef.Status.ConfigTemplateRef.Name == "" {
				return fmt.Errorf("source definition configTemplateRef missing")
			}
			if secret.Labels["config.oam.dev/type"] != sourceDef.Status.ConfigTemplateRef.Name {
				return fmt.Errorf("unexpected config type label: %q", secret.Labels["config.oam.dev/type"])
			}
			if len(secret.Data["input-properties"]) == 0 {
				return fmt.Errorf("missing input-properties in source cache secret")
			}
			var got map[string]interface{}
			if err := json.Unmarshal(secret.Data["input-properties"], &got); err != nil {
				return err
			}
			image, _ := got["image"].(string)
			if image != "nginx:1.25.0" {
				return fmt.Errorf("unexpected cached image: %v", got["image"])
			}
			return nil
		}, 60*time.Second, time.Second).Should(Succeed())
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
schema: {
  nested: {
    image: {
      repo: string
      tag:  string
    }
    meta: {
      region: string
    }
  }
}
storage: {
  key: "cluster-source"
}
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
schema: {
  resolved: {
    image:  string
    region: string
  }
}
storage: {
  key: "render-source"
}
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
schema: {
  scale: {
    replicas: int
  }
}
storage: {
  key: "scale-source"
}
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

	// Negative cases: verify the admission webhook is wired and rejects known-bad
	// source usage at kubectl apply time, with an intelligible message. These
	// complement the unit tests in pkg/webhook (which test the logic directly) by
	// proving the deny path reaches the client end-to-end.
	Context("rejects invalid source usage at admission", func() {
		// A SourceDefinition with a required string field, an optional field, and
		// an int parameter, used by the negative cases below.
		applyTypedSource := func() {
			sourceDef := &v1beta1.SourceDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: "typed-source", Namespace: namespaceName},
				Spec: v1beta1.SourceDefinitionSpec{
					Schematic: &oamcomm.Schematic{CUE: &oamcomm.CUE{Template: `
schema: {
  image:  string
  vpcId?: string
}
storage: {
  key: "typed-source"
}
output: {
  image: parameter.image
}
parameter: {
  image:    string
  replicas: int
}
`}},
				},
			}
			Expect(k8sClient.Create(ctx, sourceDef)).Should(Succeed())
		}

		newApp := func(name string, sources []v1beta1.ApplicationSource, comps []oamcomm.ApplicationComponent) *v1beta1.Application {
			return &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespaceName},
				Spec:       v1beta1.ApplicationSpec{Sources: sources, Components: comps},
			}
		}

		// spec.components is structurally required by the Application CRD, so
		// cases that exercise only source-level validation still need a valid
		// component to get past structural admission and reach the webhook.
		minimalComp := func() []oamcomm.ApplicationComponent {
			return []oamcomm.ApplicationComponent{
				{Name: "web", Type: "webservice", Properties: &runtime.RawExtension{Raw: []byte(`{"image":"nginx:1.25.0","port":80}`)}},
			}
		}

		It("denies a fromSource path not declared in the source schema", func() {
			applyTypedSource()
			app := newApp("bad-path", []v1beta1.ApplicationSource{
				{Name: "s", Type: "typed-source", Properties: &runtime.RawExtension{Raw: []byte(`{"image":"nginx","replicas":1}`)}},
			}, []oamcomm.ApplicationComponent{
				{Name: "web", Type: "webservice", Properties: &runtime.RawExtension{Raw: []byte(`{"image":{"fromSource":"s.doesNotExist"},"port":80}`)}},
			})
			err := k8sClient.Create(ctx, app)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("is not declared in schema of SourceDefinition"))
		})

		It("denies an optional schema field consumed without a default", func() {
			applyTypedSource()
			app := newApp("no-default", []v1beta1.ApplicationSource{
				{Name: "s", Type: "typed-source", Properties: &runtime.RawExtension{Raw: []byte(`{"image":"nginx","replicas":1}`)}},
			}, []oamcomm.ApplicationComponent{
				{Name: "web", Type: "webservice", Properties: &runtime.RawExtension{Raw: []byte(`{"image":{"fromSource":"s.vpcId"},"port":80}`)}},
			})
			err := k8sClient.Create(ctx, app)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("a default must be supplied via the fromSource map form"))
		})

		It("denies a forward source dependency", func() {
			applyTypedSource()
			// source at index 0 references a source declared at index 1.
			app := newApp("forward-ref", []v1beta1.ApplicationSource{
				{Name: "first", Type: "typed-source", Properties: &runtime.RawExtension{Raw: []byte(`{"image":{"fromSource":"second.image"},"replicas":1}`)}},
				{Name: "second", Type: "typed-source", Properties: &runtime.RawExtension{Raw: []byte(`{"image":"nginx","replicas":1}`)}},
			}, minimalComp())
			err := k8sClient.Create(ctx, app)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("can only depend on prior sources"))
		})

		It("denies an undeclared source property", func() {
			applyTypedSource()
			app := newApp("unknown-prop", []v1beta1.ApplicationSource{
				{Name: "s", Type: "typed-source", Properties: &runtime.RawExtension{Raw: []byte(`{"image":"nginx","replicas":1,"bogus":"x"}`)}},
			}, minimalComp())
			err := k8sClient.Create(ctx, app)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("is not declared in the parameter schema of SourceDefinition"))
		})

		It("denies a source property with an incompatible type", func() {
			applyTypedSource()
			app := newApp("bad-type", []v1beta1.ApplicationSource{
				{Name: "s", Type: "typed-source", Properties: &runtime.RawExtension{Raw: []byte(`{"image":"nginx","replicas":"three"}`)}},
			}, minimalComp())
			err := k8sClient.Create(ctx, app)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("type mismatch for parameter"))
		})
	})
})
