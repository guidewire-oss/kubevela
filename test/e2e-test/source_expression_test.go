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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	wfTypesv1alpha1 "github.com/kubevela/pkg/apis/oam/v1alpha1"

	oamcomm "github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/oam/util"
)

// exprComponentDefinition builds a ConfigMap-producing ComponentDefinition, so a
// spec can assert on rendered values without needing a workload to become ready.
func exprComponentDefinition(namespace, name, template string) *v1beta1.ComponentDefinition {
	return &v1beta1.ComponentDefinition{
		TypeMeta:   metav1.TypeMeta{Kind: "ComponentDefinition", APIVersion: "core.oam.dev/v1beta1"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1beta1.ComponentDefinitionSpec{
			Workload: oamcomm.WorkloadTypeDescriptor{
				Definition: oamcomm.WorkloadGVK{APIVersion: "v1", Kind: "ConfigMap"},
			},
			Schematic: &oamcomm.Schematic{CUE: &oamcomm.CUE{Template: template}},
		},
	}
}

func exprTraitDefinition(namespace, name, template string) *v1beta1.TraitDefinition {
	return &v1beta1.TraitDefinition{
		TypeMeta:   metav1.TypeMeta{Kind: "TraitDefinition", APIVersion: "core.oam.dev/v1beta1"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1beta1.TraitDefinitionSpec{
			AppliesToWorkloads: []string{"*"},
			Schematic:          &oamcomm.Schematic{CUE: &oamcomm.CUE{Template: template}},
		},
	}
}

// These specs cover property expressions across every surface that carries them,
// and across the type surface: scalars, arithmetic, concatenation, optional
// fields with defaults, structs, lists and nested reads.
//
// They are deliberately end-to-end rather than unit tests of the evaluator. The
// evaluator has its own tests; what these prove is the wiring - that each render
// path substitutes, that the value keeps its type all the way into the rendered
// object, and that a source read through an expression still drives resolution
// and reports status.
var _ = Describe("Source expressions across surfaces", func() {
	ctx := context.Background()

	var namespaceName string
	var ns corev1.Namespace

	// A source with one of everything: scalars of each kind, a nested struct, a
	// list, and an optional field that is genuinely absent.
	const infraSource = `
schema: {
  host:   string
  port:   int
  ratio:  float
  secure: bool
  tags:   [...string]
  meta:   {region: string, zone: string}
  note?:  string
}
$internal: {key: "infra-facts", keyInputs: []}
storage: {storageTTL: "1h"}
output: {
  host:   parameter.host
  port:   parameter.port
  ratio:  1.5
  secure: true
  tags:   ["alpha", "beta"]
  meta:   {region: "eu-west", zone: "eu-west-1a"}
}
parameter: {host: string, port: int}
`

	// A second source, whose own properties are fed from the first. Chaining is
	// what makes "sources resolve before consumers" load-bearing.
	const derivedSource = `
schema: {endpoint: string, doubled: int}
$internal: {key: "derived-ref", keyInputs: []}
output: {
  endpoint: parameter.host + ":" + "\(parameter.port)"
  doubled:  parameter.port * 2
}
parameter: {host: string, port: int}
`

	// A component whose parameter block names each type precisely, so a
	// substitution that produced the wrong one would be refused at admission
	// rather than quietly coerced.
	const probeComponent = `
import "strings"

parameter: {
  host:     string
  port:     int
  ratio:    float
  secure:   bool
  tags:     [...string]
  meta:     {region: string, zone: string}
  fallback: string
  halved:   int
}
output: {
  apiVersion: "v1"
  kind:       "ConfigMap"
  metadata: name: "expr-probe"
  data: {
    host:     parameter.host
    port:     "\(parameter.port)"
    ratio:    "\(parameter.ratio)"
    secure:   "\(parameter.secure)"
    tags:     strings.Join(parameter.tags, ",")
    region:   parameter.meta.region
    zone:     parameter.meta.zone
    fallback: parameter.fallback
    halved:   "\(parameter.halved)"
  }
}
`

	const tagTrait = `
parameter: {owner: string, where: string}
patch: {
  metadata: labels: {
    "expr-owner": parameter.owner
    "expr-where": parameter.where
  }
}
`

	applyDef := func(obj client.Object) {
		Expect(k8sClient.Create(ctx, obj)).Should(SatisfyAny(BeNil(), &util.AlreadyExistMatcher{}))
	}

	BeforeEach(func() {
		namespaceName = randomNamespaceName("source-expr-e2e")
		ns = createNamespace(ctx, namespaceName)

		applyDef(&v1beta1.SourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "infra-facts", Namespace: namespaceName},
			Spec: v1beta1.SourceDefinitionSpec{
				Schematic: &oamcomm.Schematic{CUE: &oamcomm.CUE{Template: infraSource}},
			},
		})
		applyDef(&v1beta1.SourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "derived-ref", Namespace: namespaceName},
			Spec: v1beta1.SourceDefinitionSpec{
				Schematic: &oamcomm.Schematic{CUE: &oamcomm.CUE{Template: derivedSource}},
			},
		})
	})

	AfterEach(func() {
		By("Clean up the source-expression namespace")
		Expect(k8sClient.Delete(ctx, &ns, client.PropagationPolicy(metav1.DeletePropagationBackground))).Should(BeNil())

		By("Clean up cache entries and config templates in vela-system")
		nsLabel := client.MatchingLabels{"sourcedefinition.oam.dev/namespace": namespaceName}
		Eventually(func() error {
			if err := k8sClient.DeleteAllOf(ctx, &corev1.ConfigMap{},
				client.InNamespace("vela-system"), nsLabel); err != nil {
				return err
			}
			return k8sClient.DeleteAllOf(ctx, &corev1.Secret{},
				client.InNamespace("vela-system"), nsLabel)
		}, 30*time.Second, time.Second).Should(Succeed())
	})

	configMapData := func(name string) (map[string]string, error) {
		cm := &corev1.ConfigMap{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespaceName, Name: name}, cm); err != nil {
			return nil, err
		}
		return cm.Data, nil
	}

	It("substitutes every type into a component, keeping each one's type", func() {
		applyDef(exprComponentDefinition(namespaceName, "expr-probe-comp", probeComponent))

		app := &v1beta1.Application{
			ObjectMeta: metav1.ObjectMeta{Name: "expr-types", Namespace: namespaceName},
			Spec: v1beta1.ApplicationSpec{
				Sources: []v1beta1.ApplicationSource{{
					Name:       "infra",
					Type:       "infra-facts",
					Properties: &runtime.RawExtension{Raw: []byte(`{"host":"registry.example.com","port":8080}`)},
				}},
				Components: []oamcomm.ApplicationComponent{{
					Name: "probe",
					Type: "expr-probe-comp",
					Properties: &runtime.RawExtension{Raw: []byte(`{
  "host":     "$(source.infra.host + \".internal\")",
  "port":     "$(source.infra.port + 1000)",
  "ratio":    "$(source.infra.ratio)",
  "secure":   "$(source.infra.secure)",
  "tags":     "$(source.infra.tags)",
  "meta":     "$(source.infra.meta)",
  "fallback": "$(has(source.infra.note) ? source.infra.note : \"unset\")",
  "halved":   "$(source.infra.port div 2)"
}`)},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, app)).Should(Succeed())
		verifyApplicationPhase(ctx, namespaceName, app.Name, oamcomm.ApplicationRunning)

		Eventually(func() (map[string]string, error) {
			return configMapData("expr-probe")
		}, 90*time.Second, 2*time.Second).Should(Equal(map[string]string{
			// concatenation
			"host": "registry.example.com.internal",
			// integer addition, and integer division via the infix operator
			"port":   "9080",
			"halved": "4040",
			// a float and a bool keep their own types through an int-free path
			"ratio":  "1.5",
			"secure": "true",
			// a list and a nested struct field
			"tags":   "alpha,beta",
			"region": "eu-west",
			"zone":   "eu-west-1a",
			// an optional schema field that is absent, with a default
			"fallback": "unset",
		}))
	})

	It("reports source status for a binding read only through an expression", func() {
		applyDef(exprComponentDefinition(namespaceName, "expr-status-comp", `
parameter: {host: string}
output: {
  apiVersion: "v1"
  kind:       "ConfigMap"
  metadata: name: "expr-status"
  data: host: parameter.host
}
`))

		app := &v1beta1.Application{
			ObjectMeta: metav1.ObjectMeta{Name: "expr-status", Namespace: namespaceName},
			Spec: v1beta1.ApplicationSpec{
				Sources: []v1beta1.ApplicationSource{{
					Name:       "infra",
					Type:       "infra-facts",
					Properties: &runtime.RawExtension{Raw: []byte(`{"host":"h","port":1}`)},
				}},
				Components: []oamcomm.ApplicationComponent{{
					Name:       "probe",
					Type:       "expr-status-comp",
					Properties: &runtime.RawExtension{Raw: []byte(`{"host":"$(source.infra.host)"}`)},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, app)).Should(Succeed())
		verifyApplicationPhase(ctx, namespaceName, app.Name, oamcomm.ApplicationRunning)

		// The point: an expression must drive the same resolution and the same
		// consumed-value recording status expects, or a binding used
		// only by an expression would appear unresolved.
		Eventually(func() error {
			latest := &v1beta1.Application{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(app), latest); err != nil {
				return err
			}
			for _, svc := range latest.Status.Services {
				for _, src := range svc.Sources {
					if src.Name != "infra" {
						continue
					}
					if src.Config == "" {
						return fmt.Errorf("no cache entry recorded for a binding read via an expression")
					}
					return nil
				}
			}
			return fmt.Errorf("source %q missing from status", "infra")
		}, 90*time.Second, 2*time.Second).Should(Succeed())
	})

	It("substitutes expressions in trait properties", func() {
		applyDef(exprComponentDefinition(namespaceName, "expr-trait-comp", `
parameter: {host: string}
output: {
  apiVersion: "v1"
  kind:       "ConfigMap"
  metadata: name: "expr-trait"
  data: host: parameter.host
}
`))
		applyDef(exprTraitDefinition(namespaceName, "expr-tag-trait", tagTrait))

		app := &v1beta1.Application{
			ObjectMeta: metav1.ObjectMeta{
				Name: "expr-trait", Namespace: namespaceName,
				Labels: map[string]string{"team": "payments"},
			},
			Spec: v1beta1.ApplicationSpec{
				Sources: []v1beta1.ApplicationSource{{
					Name:       "infra",
					Type:       "infra-facts",
					Properties: &runtime.RawExtension{Raw: []byte(`{"host":"h","port":1}`)},
				}},
				Components: []oamcomm.ApplicationComponent{{
					Name:       "probe",
					Type:       "expr-trait-comp",
					Properties: &runtime.RawExtension{Raw: []byte(`{"host":"$(source.infra.meta.region)"}`)},
					Traits: []oamcomm.ApplicationTrait{{
						Type: "expr-tag-trait",
						// One from a source, one from context, in the same block.
						Properties: &runtime.RawExtension{Raw: []byte(`{
  "owner": "$(\"team\" in context.appLabels ? context.appLabels[\"team\"] : \"unowned\")",
  "where": "$(source.infra.meta.zone + \"--\" + context.namespace)"
}`)},
					}},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, app)).Should(Succeed())
		verifyApplicationPhase(ctx, namespaceName, app.Name, oamcomm.ApplicationRunning)

		Eventually(func() (map[string]string, error) {
			cm := &corev1.ConfigMap{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespaceName, Name: "expr-trait"}, cm); err != nil {
				return nil, err
			}
			return cm.Labels, nil
		}, 90*time.Second, 2*time.Second).Should(SatisfyAll(
			HaveKeyWithValue("expr-owner", "payments"),
			// "/" is legal in a label *key* prefix but not in a value, so the
			// expression joins with a separator a label value can hold.
			HaveKeyWithValue("expr-where", "eu-west-1a--"+namespaceName),
		))
	})

	It("substitutes expressions in workflow step properties", func() {
		applyDef(exprComponentDefinition(namespaceName, "expr-wf-comp", `
parameter: {}
output: {
  apiVersion: "v1"
  kind:       "ConfigMap"
  metadata: name: "expr-wf-placeholder"
  data: ok: "yes"
}
`))

		app := &v1beta1.Application{
			ObjectMeta: metav1.ObjectMeta{Name: "expr-wf", Namespace: namespaceName},
			Spec: v1beta1.ApplicationSpec{
				Sources: []v1beta1.ApplicationSource{{
					Name:       "infra",
					Type:       "infra-facts",
					Properties: &runtime.RawExtension{Raw: []byte(`{"host":"wf.example.com","port":7000}`)},
				}},
				Components: []oamcomm.ApplicationComponent{{
					Name: "placeholder", Type: "expr-wf-comp",
					Properties: &runtime.RawExtension{Raw: []byte(`{}`)},
				}},
				Workflow: &v1beta1.Workflow{
					Steps: []wfTypesv1alpha1.WorkflowStep{{
						WorkflowStepBase: wfTypesv1alpha1.WorkflowStepBase{
							Name: "write", Type: "apply-object",
							Properties: &runtime.RawExtension{Raw: []byte(`{
  "value": {
    "apiVersion": "v1",
    "kind": "ConfigMap",
    "metadata": {"name": "expr-wf-result", "namespace": "` + namespaceName + `"},
    "data": {
      "endpoint": "$(source.infra.host + \":\" + \"\\(source.infra.port)\")",
      "app":      "$(context.appName)"
    }
  }
}`)},
						},
					}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, app)).Should(Succeed())

		Eventually(func() (map[string]string, error) {
			return configMapData("expr-wf-result")
		}, 120*time.Second, 2*time.Second).Should(Equal(map[string]string{
			"endpoint": "wf.example.com:7000",
			"app":      "expr-wf",
		}))
	})

	It("chains sources, with the second reading the first through an expression", func() {
		applyDef(exprComponentDefinition(namespaceName, "expr-chain-comp", `
parameter: {endpoint: string, doubled: int}
output: {
  apiVersion: "v1"
  kind:       "ConfigMap"
  metadata: name: "expr-chain"
  data: {
    endpoint: parameter.endpoint
    doubled:  "\(parameter.doubled)"
  }
}
`))

		app := &v1beta1.Application{
			ObjectMeta: metav1.ObjectMeta{Name: "expr-chain", Namespace: namespaceName},
			Spec: v1beta1.ApplicationSpec{
				Sources: []v1beta1.ApplicationSource{
					{
						Name:       "infra",
						Type:       "infra-facts",
						Properties: &runtime.RawExtension{Raw: []byte(`{"host":"chain.example.com","port":9000}`)},
					},
					{
						// The chained source reads the first through expressions,
						// which is what proves ordering still holds when the
						// dependency is expressed rather than declared.
						Name: "derived",
						Type: "derived-ref",
						Properties: &runtime.RawExtension{Raw: []byte(`{
  "host": "$(source.infra.host)",
  "port": "$(source.infra.port)"
}`)},
					},
				},
				Components: []oamcomm.ApplicationComponent{{
					Name: "probe",
					Type: "expr-chain-comp",
					Properties: &runtime.RawExtension{Raw: []byte(`{
  "endpoint": "$(source.derived.endpoint)",
  "doubled":  "$(source.derived.doubled)"
}`)},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, app)).Should(Succeed())
		verifyApplicationPhase(ctx, namespaceName, app.Name, oamcomm.ApplicationRunning)

		Eventually(func() (map[string]string, error) {
			return configMapData("expr-chain")
		}, 90*time.Second, 2*time.Second).Should(Equal(map[string]string{
			"endpoint": "chain.example.com:9000",
			"doubled":  "18000",
		}))
	})

	It("resolves a source per consuming component, and refuses it where no component exists", func() {
		// The capability the caller-identity keyed fields were added for. A source
		// reading context.componentName gets one cache entry per component, and is
		// consumable only where a component is being rendered - which the
		// surface-compatibility check enforces per binding.
		applyDef(&v1beta1.SourceDefinition{
			TypeMeta:   metav1.TypeMeta{Kind: "SourceDefinition", APIVersion: "core.oam.dev/v1beta1"},
			ObjectMeta: metav1.ObjectMeta{Name: "per-comp-src", Namespace: namespaceName},
			Spec: v1beta1.SourceDefinitionSpec{
				Schematic: &oamcomm.Schematic{CUE: &oamcomm.CUE{Template: `
schema: {label: string}
$internal: {
	key: "per-comp-src-\(context.namespace)-\(context.componentName)"
	keyInputs: ["namespace", "componentName"]
}
storage: {storageTTL: "10m"}
output: {label: "\(context.componentName)@\(context.namespace)"}
parameter: {}
`}},
			},
		})
		applyDef(exprComponentDefinition(namespaceName, "per-comp-consumer", `
parameter: {who: string}
output: {
  apiVersion: "v1"
  kind:       "ConfigMap"
  metadata: name: context.name
  data: who: parameter.who
}
`))

		// Two components share one binding and must each resolve to their own
		// value - the point of keying on the component.
		app := &v1beta1.Application{
			ObjectMeta: metav1.ObjectMeta{Name: "per-comp-app", Namespace: namespaceName},
			Spec: v1beta1.ApplicationSpec{
				Sources: []v1beta1.ApplicationSource{{
					Name: "mine", Type: "per-comp-src",
					Properties: &runtime.RawExtension{Raw: []byte(`{}`)},
				}},
				Components: []oamcomm.ApplicationComponent{
					{
						Name: "alpha", Type: "per-comp-consumer",
						Properties: &runtime.RawExtension{Raw: []byte(`{"who":"$(source.mine.label)"}`)},
					},
					{
						Name: "beta", Type: "per-comp-consumer",
						Properties: &runtime.RawExtension{Raw: []byte(`{"who":"$(source.mine.label)"}`)},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, app)).Should(Succeed())

		Eventually(func() (map[string]string, error) {
			return configMapData("alpha")
		}, 120*time.Second, 2*time.Second).Should(
			HaveKeyWithValue("who", "alpha@"+namespaceName))
		Eventually(func() (map[string]string, error) {
			return configMapData("beta")
		}, 120*time.Second, 2*time.Second).Should(
			HaveKeyWithValue("who", "beta@"+namespaceName))

		// The same binding read from a workflow step cannot resolve: no component
		// is rendered there. Only that reference is refused - the component reads
		// above are correct and must not be dragged down with it.
		bad := &v1beta1.Application{
			ObjectMeta: metav1.ObjectMeta{Name: "per-comp-bad", Namespace: namespaceName},
			Spec: v1beta1.ApplicationSpec{
				Sources: []v1beta1.ApplicationSource{{
					Name: "mine", Type: "per-comp-src",
					Properties: &runtime.RawExtension{Raw: []byte(`{}`)},
				}},
				Components: []oamcomm.ApplicationComponent{{
					Name: "alpha", Type: "per-comp-consumer",
					Properties: &runtime.RawExtension{Raw: []byte(`{"who":"$(source.mine.label)"}`)},
				}},
				Workflow: &v1beta1.Workflow{Steps: []wfTypesv1alpha1.WorkflowStep{{
					WorkflowStepBase: wfTypesv1alpha1.WorkflowStepBase{
						Name: "s", Type: "step-group",
						Properties: &runtime.RawExtension{Raw: []byte(`{"label":"$(source.mine.label)"}`)},
					},
				}}},
			},
		}
		err := k8sClient.Create(ctx, bad)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("context.componentName"))
		Expect(err.Error()).To(ContainSubstring("spec.workflow"))
		Expect(err.Error()).NotTo(ContainSubstring("spec.components"),
			"the component read resolves; only the workflow-step read is wrong")
	})

	It("resolves sources and context in a resource-rendering policy", func() {
		// The third policy kind, and the only one that can resolve a source: a
		// PolicyDefinition with a CUE template renders through the same engine a
		// component does, so the resolver is in hand by the time its properties
		// are substituted.
		//
		// It reads context.cluster deliberately. Most sources do a cluster-scoped
		// lookup, so they read that field and key on it - a policy surface without
		// it could consume almost no source at all. It is supplied as the hub,
		// which is where a policy's manifests are dispatched.
		applyDef(&v1beta1.PolicyDefinition{
			TypeMeta:   metav1.TypeMeta{Kind: "PolicyDefinition", APIVersion: "core.oam.dev/v1beta1"},
			ObjectMeta: metav1.ObjectMeta{Name: "expr-render-policy", Namespace: namespaceName},
			Spec: v1beta1.PolicyDefinitionSpec{
				Schematic: &oamcomm.Schematic{CUE: &oamcomm.CUE{Template: `
parameter: {host: string, where: string, who: string}
output: {
  apiVersion: "v1"
  kind:       "ConfigMap"
  metadata: name: "expr-render-policy-out"
  data: {
    host:  parameter.host
    where: parameter.where
    who:   parameter.who
  }
}
`}},
			},
		})
		applyDef(exprComponentDefinition(namespaceName, "expr-render-comp", `
parameter: {}
output: {
  apiVersion: "v1"
  kind:       "ConfigMap"
  metadata: name: "expr-render-placeholder"
  data: ok: "yes"
}
`))

		app := &v1beta1.Application{
			ObjectMeta: metav1.ObjectMeta{Name: "expr-render-pol", Namespace: namespaceName},
			Spec: v1beta1.ApplicationSpec{
				Sources: []v1beta1.ApplicationSource{{
					Name:       "infra",
					Type:       "infra-facts",
					Properties: &runtime.RawExtension{Raw: []byte(`{"host":"db.internal","port":5432}`)},
				}},
				Components: []oamcomm.ApplicationComponent{{
					Name: "placeholder", Type: "expr-render-comp",
					Properties: &runtime.RawExtension{Raw: []byte(`{}`)},
				}},
				Policies: []v1beta1.AppPolicy{{
					Name: "pinned", Type: "expr-render-policy",
					Properties: &runtime.RawExtension{Raw: []byte(`{
  "host":  "$(source.infra.host)",
  "where": "$(context.cluster)",
  "who":   "$(context.policyName + \"/\" + context.policyType)"
}`)},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, app)).Should(Succeed())

		Eventually(func() (map[string]string, error) {
			return configMapData("expr-render-policy-out")
		}, 120*time.Second, 2*time.Second).Should(SatisfyAll(
			// The source resolved - this is the value the SourceDefinition
			// returned, not the literal text of the expression.
			HaveKeyWithValue("host", "db.internal"),
			// And the policy's own identity and placement.
			HaveKeyWithValue("where", "local"),
			HaveKeyWithValue("who", "pinned/expr-render-policy"),
		))
	})

	It("substitutes context expressions in an Application-scoped policy", func() {
		// Scoped policies render before the appfile exists, so they can read
		// context but not sources. This is the surface that has half the feature
		// rather than none of it.
		//
		// Requires --feature-gates=EnableApplicationScopedPolicies=true; without
		// it the whole path is skipped and this spec would pass vacuously, so it
		// asserts on the resulting labels rather than on the app merely running.
		applyDef(&v1beta1.PolicyDefinition{
			TypeMeta:   metav1.TypeMeta{Kind: "PolicyDefinition", APIVersion: "core.oam.dev/v1beta1"},
			ObjectMeta: metav1.ObjectMeta{Name: "expr-scope-policy", Namespace: namespaceName},
			Spec: v1beta1.PolicyDefinitionSpec{
				Scope: v1beta1.ApplicationScope,
				Schematic: &oamcomm.Schematic{CUE: &oamcomm.CUE{Template: `
parameter: {owner: string, where: string}
output: labels: {
  "policy-owner": parameter.owner
  "policy-where": parameter.where
}
`}},
			},
		})
		applyDef(exprComponentDefinition(namespaceName, "expr-policy-comp", `
parameter: {}
output: {
  apiVersion: "v1"
  kind:       "ConfigMap"
  metadata: name: "expr-policy-placeholder"
  data: ok: "yes"
}
`))

		app := &v1beta1.Application{
			ObjectMeta: metav1.ObjectMeta{
				Name: "expr-policy", Namespace: namespaceName,
				Labels: map[string]string{"team": "payments"},
			},
			Spec: v1beta1.ApplicationSpec{
				Components: []oamcomm.ApplicationComponent{{
					Name: "placeholder", Type: "expr-policy-comp",
					Properties: &runtime.RawExtension{Raw: []byte(`{}`)},
				}},
				Policies: []v1beta1.AppPolicy{{
					Name: "tagger", Type: "expr-scope-policy",
					Properties: &runtime.RawExtension{Raw: []byte(`{
  "owner": "$(\"team\" in context.appLabels ? context.appLabels[\"team\"] : \"unowned\")",
  "where": "$(context.appName + \"--\" + context.namespace)"
}`)},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, app)).Should(Succeed())

		Eventually(func() (map[string]string, error) {
			latest := &v1beta1.Application{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(app), latest); err != nil {
				return nil, err
			}
			return latest.Labels, nil
		}, 120*time.Second, 2*time.Second).Should(SatisfyAll(
			HaveKeyWithValue("policy-owner", "payments"),
			HaveKeyWithValue("policy-where", "expr-policy--"+namespaceName),
		))
	})

	Context("rejects at admission what would fail at render", func() {
		expectRejected := func(props string, wants ...string) {
			app := &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "expr-bad", Namespace: namespaceName},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{{
						Name:       "infra",
						Type:       "infra-facts",
						Properties: &runtime.RawExtension{Raw: []byte(`{"host":"h","port":1}`)},
					}},
					Components: []oamcomm.ApplicationComponent{{
						Name: "probe", Type: "expr-probe-comp",
						Properties: &runtime.RawExtension{Raw: []byte(props)},
					}},
				},
			}
			err := k8sClient.Create(ctx, app)
			Expect(err).To(HaveOccurred())
			for _, w := range wants {
				Expect(err.Error()).To(ContainSubstring(w))
			}
		}

		BeforeEach(func() {
			applyDef(exprComponentDefinition(namespaceName, "expr-probe-comp", probeComponent))
			// The scoped policy the refusal table below uses. Its spec.scope is
			// what makes it scoped rather than resource-rendering, and that
			// distinction now decides whether a source may be read.
			applyDef(&v1beta1.PolicyDefinition{
				TypeMeta:   metav1.TypeMeta{Kind: "PolicyDefinition", APIVersion: "core.oam.dev/v1beta1"},
				ObjectMeta: metav1.ObjectMeta{Name: "expr-scope-policy", Namespace: namespaceName},
				Spec: v1beta1.PolicyDefinitionSpec{
					Scope: v1beta1.ApplicationScope,
					Schematic: &oamcomm.Schematic{CUE: &oamcomm.CUE{Template: `
parameter: {owner: string}
output: labels: "policy-owner": parameter.owner
`}},
				},
			})
		})

		It("denies a string expression feeding an int parameter", func() {
			expectRejected(`{"port":"$(source.infra.host)"}`, "type mismatch")
		})

		It("denies a struct expression feeding a string parameter", func() {
			expectRejected(`{"host":"$(source.infra.meta)"}`, "type mismatch")
		})

		It("denies a struct combined with text, which has no string form", func() {
			expectRejected(`{"host":"$(source.infra.meta)-x"}`, "cannot be combined with text")
		})

		It("denies an optional field with no default feeding a required parameter", func() {
			expectRejected(`{"host":"$(source.infra.note)"}`, "may be absent")
		})

		It("denies a field the source schema does not declare", func() {
			expectRejected(`{"host":"$(source.infra.missing)"}`, "not declared")
		})

		It("denies an identifier outside the sandbox", func() {
			expectRejected(`{"host":"$(parameter.host)"}`, "unknown identifier")
		})

		// Whether a policy may read a source depends on which kind it is, so both
		// refusing kinds are asserted. A resource-rendering PolicyDefinition is
		// deliberately absent from this list: it renders through the same engine a
		// component does, so it resolves sources, and the spec above proves it.
		//
		// The scoped definition is created here rather than relied on from another
		// spec. It used to be incidental - every policy refused a source, so the
		// type never had to resolve to anything. Now the type decides the answer,
		// and a policy whose definition is missing is classified as rendered.
		DescribeTable("denies a policy that cannot resolve a source",
			func(policyType, props string) {
				app := &v1beta1.Application{
					ObjectMeta: metav1.ObjectMeta{Name: "expr-bad-policy", Namespace: namespaceName},
					Spec: v1beta1.ApplicationSpec{
						Sources: []v1beta1.ApplicationSource{{
							Name:       "infra",
							Type:       "infra-facts",
							Properties: &runtime.RawExtension{Raw: []byte(`{"host":"h","port":1}`)},
						}},
						Components: []oamcomm.ApplicationComponent{{
							Name: "probe", Type: "expr-probe-comp",
							Properties: &runtime.RawExtension{Raw: []byte(`{"host":"$(source.infra.host)"}`)},
						}},
						Policies: []v1beta1.AppPolicy{{
							Name: "p", Type: policyType,
							Properties: &runtime.RawExtension{Raw: []byte(props)},
						}},
					},
				}
				err := k8sClient.Create(ctx, app)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("cannot be read here"))
			},
			// Renders before the appfile exists, so there is no spec.sources[] yet.
			Entry("an Application-scoped policy", "expr-scope-policy", `{"owner":"$(source.infra.host)"}`),
			// Read straight off the appfile by a provider - nothing renders it, so
			// there is no resolver to reach a source through.
			Entry("a built-in policy", "override", `{"components":["$(source.infra.host)"]}`),
		)
	})
})
