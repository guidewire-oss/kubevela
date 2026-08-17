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

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/utils/common"
	"github.com/oam-dev/kubevela/pkg/utils/util"
)

// spokeClusterScheme returns a scheme with the SpokeCluster types registered.
func spokeClusterScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	Expect(v1beta1.AddToScheme(s)).To(Succeed())
	Expect(corev1.AddToScheme(s)).To(Succeed())
	return s
}

// newSpokeCluster builds a SpokeCluster fixture for tests.
func newSpokeCluster(name, namespace string, mode v1beta1.SpokeClusterMode, auth v1beta1.CredentialType) *v1beta1.SpokeCluster {
	return &v1beta1.SpokeCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1beta1.SpokeClusterSpec{
			Mode:       mode,
			Credential: v1beta1.CredentialSpec{Type: auth},
		},
	}
}

// fakeClientWith builds a fake client seeded with the given SpokeCluster objects.
func fakeClientWith(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(spokeClusterScheme()).WithObjects(objs...).Build()
}

// noMatchClient returns a fake client whose list/get report the SpokeCluster kind as not
// installed, simulating a cluster where the vela-core chart was never applied. Note this is
// not what the feature gate being off looks like: the chart installs crds/ unconditionally,
// so with the gate off the kind still resolves and only the status stays empty.
func noMatchClient() client.Client {
	noMatch := &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "core.oam.dev", Kind: "SpokeCluster"}}
	return fake.NewClientBuilder().WithScheme(spokeClusterScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return noMatch
			},
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return noMatch
			},
		}).Build()
}

// forbiddenListClient returns a fake client whose list is rejected, simulating a caller
// who holds namespaced rights on spokeclusters but no cluster-wide list permission.
func forbiddenListClient() client.Client {
	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Group: "core.oam.dev", Resource: "spokeclusters"}, "",
		fmt.Errorf("User %q cannot list resource %q at the cluster scope", "team-a-dev", "spokeclusters"))
	return fake.NewClientBuilder().WithScheme(spokeClusterScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return forbidden
			},
		}).Build()
}

// forbiddenListGetClient returns a fake client whose get is a plain NotFound and whose
// list is Forbidden, simulating the namespace-scoped caller that show's cluster-wide
// courtesy lookup must not fail on.
func forbiddenListGetClient() client.Client {
	return fake.NewClientBuilder().WithScheme(spokeClusterScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: "core.oam.dev", Resource: "spokeclusters"}, "",
					fmt.Errorf("cannot list at the cluster scope"))
			},
		}).Build()
}

// subCommand returns the direct subcommand of cmd with the given Use name, or nil.
func subCommand(cmd *cobra.Command, use string) *cobra.Command {
	for _, sub := range cmd.Commands() {
		if sub.Name() == use {
			return sub
		}
	}
	return nil
}

var _ = Describe("vela cluster spokes command wiring", func() {
	It("registers the spokes group with aliases and all subcommands", func() {
		cmd := NewSpokeClusterCommandGroup(&common.Args{})
		Expect(cmd.Name()).To(Equal("spokes"))
		Expect(cmd.Aliases).To(ConsistOf("spoke", "spokecluster", "spokeclusters"))
		list := subCommand(cmd, "list")
		Expect(list).ToNot(BeNil(), "expected a list subcommand")
		Expect(subCommand(cmd, "show")).ToNot(BeNil(), "expected a show subcommand")
		Expect(subCommand(cmd, "create")).ToNot(BeNil(), "expected a create subcommand")
		Expect(subCommand(cmd, "detach")).ToNot(BeNil(), "expected a detach subcommand")
		Expect(list.Aliases).To(ContainElement("ls"), "list must accept the ls alias")
	})

	It("hooks into vela cluster and leaves the legacy list command in place", func() {
		ioStreams := util.IOStreams{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr}
		clusterCmd := ClusterCommandGroup(nil, "", common.Args{}, ioStreams)
		Expect(subCommand(clusterCmd, "spokes")).ToNot(BeNil(), "expected spokes group under vela cluster")
		Expect(subCommand(clusterCmd, "list")).ToNot(BeNil(), "expected legacy list command to remain")
	})

	It("gives the spokes group a gateway-independent pre-run that subcommands inherit", func() {
		cmd := NewSpokeClusterCommandGroup(&common.Args{})
		Expect(cmd.PersistentPreRunE).ToNot(BeNil(), "spokes group must set its own PersistentPreRunE")
		Expect(cmd.PersistentPreRunE(cmd, nil)).To(Succeed())
		for _, sub := range cmd.Commands() {
			Expect(sub.PersistentPreRunE).To(BeNil(), "subcommand %q should inherit the group pre-run", sub.Name())
		}
	})

	It("defaults the show namespace to vela-system", func() {
		flag := newSpokeClusterShowCommand(&common.Args{}).Flags().Lookup("namespace")
		Expect(flag).ToNot(BeNil())
		Expect(flag.DefValue).To(Equal("vela-system"))
	})

	// list used to default to every namespace while show, create and detach defaulted to
	// vela-system, so a name copied out of the list often failed the next show. Every verb
	// now starts in the same place.
	It("defaults the list namespace to vela-system, matching the other verbs", func() {
		flag := newSpokeClusterListCommand(&common.Args{}).Flags().Lookup("namespace")
		Expect(flag).ToNot(BeNil())
		Expect(flag.DefValue).To(Equal("vela-system"))
	})

	It("offers -A for the cluster-wide view, off by default", func() {
		flags := newSpokeClusterListCommand(&common.Args{}).Flags()
		flag := flags.Lookup("all-namespaces")
		Expect(flag).ToNot(BeNil())
		Expect(flag.DefValue).To(Equal("false"))
		Expect(flag.Shorthand).To(Equal("A"))
	})

	It("refuses -n together with -A rather than silently ignoring one", func() {
		cmd := newSpokeClusterListCommand(&common.Args{})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"-n", "team-a", "-A"})
		err := cmd.Execute()
		Expect(err).To(HaveOccurred())
		// Cobra's own wording for a mutually exclusive flag group; it names both flags.
		Expect(err.Error()).To(ContainSubstring("namespace all-namespaces"))
		Expect(err.Error()).To(ContainSubstring("none of the others can be"))
	})

	It("defaults the timeout flag to 30s on all commands", func() {
		for _, cmd := range []*cobra.Command{
			newSpokeClusterListCommand(&common.Args{}),
			newSpokeClusterShowCommand(&common.Args{}),
			newSpokeClusterCreateCommand(&common.Args{}),
			newSpokeClusterDetachCommand(&common.Args{}),
		} {
			flag := cmd.Flags().Lookup("timeout")
			Expect(flag).ToNot(BeNil(), "%s must have a --timeout flag", cmd.Name())
			Expect(flag.DefValue).To(Equal("30s"))
		}
	})
})

var _ = Describe("vela cluster spokes list", func() {
	ctx := context.Background()

	newPopulated := func() *v1beta1.SpokeCluster {
		sc := newSpokeCluster("beta", "vela-system", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeAWS)
		sc.Status = v1beta1.SpokeClusterStatus{
			Connection:  v1beta1.ConnectionStateConnected,
			ClusterInfo: &v1beta1.SpokeClusterInfo{KubernetesVersion: "v1.29.0", Platform: "eks", NodeCount: 3},
		}
		return sc
	}

	It("renders the columns, sorts by name, and fills placeholders", func() {
		// "alpha" is declared second but must sort first.
		unpopulated := newSpokeCluster("alpha", "default", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeKubeconfig)
		var buf bytes.Buffer
		Expect(runSpokeClusterList(ctx, fakeClientWith(newPopulated(), unpopulated), "", true, &buf, "table")).To(Succeed())
		out := buf.String()

		for _, header := range []string{"NAME", "NAMESPACE", "MODE", "AUTH", "VERSION", "NODES", "PLATFORM", "STATUS"} {
			Expect(out).To(ContainSubstring(header))
		}
		Expect(out).To(ContainSubstring("v1.29.0"))
		Expect(out).To(ContainSubstring("eks"))
		Expect(out).To(ContainSubstring("Connected"))
		// Unpopulated: missing clusterInfo renders "-", empty connection renders Unknown.
		Expect(out).To(ContainSubstring("Unknown"))
		Expect(out).To(ContainSubstring("-"))
		Expect(bytes.Index(buf.Bytes(), []byte("alpha"))).To(BeNumerically("<", bytes.Index(buf.Bytes(), []byte("beta"))))
	})

	It("renders a zero node count as dash, not 0", func() {
		zeroNodes := newSpokeCluster("zero", "vela-system", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeAWS)
		zeroNodes.Status = v1beta1.SpokeClusterStatus{
			ClusterInfo: &v1beta1.SpokeClusterInfo{KubernetesVersion: "v1.30.0", NodeCount: 0},
		}
		var buf bytes.Buffer
		Expect(runSpokeClusterList(ctx, fakeClientWith(zeroNodes), "vela-system", false, &buf, "table")).To(Succeed())
		Expect(buf.String()).ToNot(ContainSubstring(" 0 "))
	})

	It("restricts results when a namespace is given", func() {
		unpopulated := newSpokeCluster("alpha", "default", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeKubeconfig)
		var buf bytes.Buffer
		Expect(runSpokeClusterList(ctx, fakeClientWith(newPopulated(), unpopulated), "default", false, &buf, "table")).To(Succeed())
		Expect(buf.String()).To(ContainSubstring("alpha"))
		Expect(buf.String()).ToNot(ContainSubstring("beta"))
	})

	It("scopes to the given namespace even though other namespaces hold spokes", func() {
		elsewhere := newSpokeCluster("alpha", "team-a", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeKubeconfig)
		var buf bytes.Buffer
		Expect(runSpokeClusterList(ctx, fakeClientWith(newPopulated(), elsewhere), "vela-system", false, &buf, "table")).To(Succeed())
		Expect(buf.String()).To(ContainSubstring("beta"))
		Expect(buf.String()).ToNot(ContainSubstring("alpha"))
	})

	It("spans namespaces when allNamespaces is set", func() {
		elsewhere := newSpokeCluster("alpha", "team-a", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeKubeconfig)
		var buf bytes.Buffer
		Expect(runSpokeClusterList(ctx, fakeClientWith(newPopulated(), elsewhere), "", true, &buf, "table")).To(Succeed())
		Expect(buf.String()).To(ContainSubstring("beta"))
		Expect(buf.String()).To(ContainSubstring("alpha"))
	})

	// Defensive: an empty namespace with allNamespaces unset would otherwise list
	// cluster-wide, which is the behaviour we just moved behind -A.
	It("falls back to vela-system when neither a namespace nor -A is given", func() {
		elsewhere := newSpokeCluster("alpha", "team-a", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeKubeconfig)
		var buf bytes.Buffer
		Expect(runSpokeClusterList(ctx, fakeClientWith(newPopulated(), elsewhere), "", false, &buf, "table")).To(Succeed())
		Expect(buf.String()).To(ContainSubstring("beta"))
		Expect(buf.String()).ToNot(ContainSubstring("alpha"))
	})

	It("prints No SpokeCluster found when empty", func() {
		var buf bytes.Buffer
		Expect(runSpokeClusterList(ctx, fakeClientWith(), "vela-system", false, &buf, "table")).To(Succeed())
		Expect(buf.String()).To(ContainSubstring("No SpokeCluster found."))
	})

	// A namespace-scoped caller has no cluster-wide list, and the raw apiserver message
	// says nothing about the flag that would work for them.
	It("suggests -n when a cluster-wide list is forbidden", func() {
		var buf bytes.Buffer
		err := runSpokeClusterList(ctx, forbiddenListClient(), "", true, &buf, "table")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("-n"))
		Expect(err.Error()).To(ContainSubstring("cluster-wide"))
	})

	It("does not add the -n hint when the namespaced list is the one forbidden", func() {
		var buf bytes.Buffer
		err := runSpokeClusterList(ctx, forbiddenListClient(), "team-a", false, &buf, "table")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).ToNot(ContainSubstring("retry with -n"))
	})
})

var _ = Describe("vela cluster spokes show", func() {
	ctx := context.Background()

	It("prints the spec summary", func() {
		sc := newSpokeCluster("prod", "vela-system", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeAWS)
		sc.Status.Connection = v1beta1.ConnectionStateConnected
		var buf bytes.Buffer
		Expect(runSpokeClusterShow(ctx, fakeClientWith(sc), "vela-system", "prod", &buf, "table")).To(Succeed())
		out := buf.String()
		Expect(out).To(ContainSubstring("prod"))
		Expect(out).To(ContainSubstring("vela-system"))
		Expect(out).To(ContainSubstring("connect"))
		Expect(out).To(ContainSubstring("aws"))
		Expect(out).To(ContainSubstring("Connected"))
	})

	It("defaults the connection to Unknown when unset", func() {
		sc := newSpokeCluster("prod", "vela-system", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeKubeconfig)
		var buf bytes.Buffer
		Expect(runSpokeClusterShow(ctx, fakeClientWith(sc), "vela-system", "prod", &buf, "table")).To(Succeed())
		Expect(buf.String()).To(ContainSubstring("Unknown"))
	})

	It("prints the Cluster Info block only when clusterInfo is populated", func() {
		withInfo := newSpokeCluster("withinfo", "vela-system", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeAWS)
		withInfo.Status.ClusterInfo = &v1beta1.SpokeClusterInfo{
			KubernetesVersion: "v1.29.0", Platform: "eks", Region: "us-west-2",
			NodeCount: 5, TotalCPU: "16", TotalMemory: "64Gi", APIServerEndpoint: "https://api.example",
		}
		var withBuf bytes.Buffer
		Expect(runSpokeClusterShow(ctx, fakeClientWith(withInfo), "vela-system", "withinfo", &withBuf, "table")).To(Succeed())
		out := withBuf.String()
		Expect(out).To(ContainSubstring("Cluster Info"))
		Expect(out).To(ContainSubstring("v1.29.0"))
		Expect(out).To(ContainSubstring("us-west-2"))
		Expect(out).To(ContainSubstring("64Gi"))
		Expect(out).To(ContainSubstring("https://api.example"))

		noInfo := newSpokeCluster("noinfo", "vela-system", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeAWS)
		var noBuf bytes.Buffer
		Expect(runSpokeClusterShow(ctx, fakeClientWith(noInfo), "vela-system", "noinfo", &noBuf, "table")).To(Succeed())
		Expect(noBuf.String()).ToNot(ContainSubstring("Cluster Info"))
	})

	It("prints the Conditions table only when conditions exist", func() {
		withCond := newSpokeCluster("withcond", "vela-system", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeAWS)
		withCond.Status.Conditions = []metav1.Condition{{
			Type: "Connected", Status: metav1.ConditionTrue, Reason: "ProbeOK", Message: "reachable",
		}}
		var withBuf bytes.Buffer
		Expect(runSpokeClusterShow(ctx, fakeClientWith(withCond), "vela-system", "withcond", &withBuf, "table")).To(Succeed())
		out := withBuf.String()
		Expect(out).To(ContainSubstring("Conditions"))
		Expect(out).To(ContainSubstring("TYPE"))
		Expect(out).To(ContainSubstring("ProbeOK"))
		Expect(out).To(ContainSubstring("reachable"))

		noCond := newSpokeCluster("nocond", "vela-system", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeAWS)
		var noBuf bytes.Buffer
		Expect(runSpokeClusterShow(ctx, fakeClientWith(noCond), "vela-system", "nocond", &noBuf, "table")).To(Succeed())
		Expect(noBuf.String()).ToNot(ContainSubstring("Conditions"))
	})

	// A name that exists nowhere gets the plain message. No search advice: suggesting a
	// cluster-wide list would send a namespace-scoped caller at a request they cannot make.
	It("returns a namespaced message when the SpokeCluster is not found", func() {
		var buf bytes.Buffer
		err := runSpokeClusterShow(ctx, fakeClientWith(), "vela-system", "ghost", &buf, "table")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("SpokeCluster vela-system/ghost not found"))
		Expect(err.Error()).ToNot(ContainSubstring("-A"))
	})

	// The common case: a name copied out of a listing that spanned namespaces. Naming the
	// namespace beats telling the caller to go looking for it.
	It("names the namespace it actually lives in", func() {
		elsewhere := newSpokeCluster("tenant-spoke", "team-a", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeKubeconfig)
		var buf bytes.Buffer
		err := runSpokeClusterShow(ctx, fakeClientWith(elsewhere), "vela-system", "tenant-spoke", &buf, "table")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not found in vela-system"))
		Expect(err.Error()).To(ContainSubstring("it is in team-a"))
		Expect(err.Error()).To(ContainSubstring("-n team-a"))
	})

	It("lists every namespace when the name is not unique", func() {
		a := newSpokeCluster("dup", "team-a", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeKubeconfig)
		b := newSpokeCluster("dup", "team-b", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeKubeconfig)
		var buf bytes.Buffer
		err := runSpokeClusterShow(ctx, fakeClientWith(a, b), "vela-system", "dup", &buf, "table")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("team-a"))
		Expect(err.Error()).To(ContainSubstring("team-b"))
	})

	// The lookup is a courtesy. A caller without cluster-wide list still gets a clean
	// answer rather than a wrapped authorization error from a call they did not make.
	It("falls back to the plain message when the cluster-wide lookup is forbidden", func() {
		var buf bytes.Buffer
		err := runSpokeClusterShow(ctx, forbiddenListGetClient(), "vela-system", "tenant-spoke", &buf, "table")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("SpokeCluster vela-system/tenant-spoke not found"))
		Expect(err.Error()).ToNot(ContainSubstring("forbidden"))
	})
})

var _ = Describe("vela cluster spokes output formats", func() {
	ctx := context.Background()

	Context("list", func() {
		var cli client.Client
		BeforeEach(func() {
			a := newSpokeCluster("alpha", "vela-system", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeAWS)
			a.Status.ClusterInfo = &v1beta1.SpokeClusterInfo{Region: "us-west-2", APIServerEndpoint: "https://a", TotalCPU: "8", TotalMemory: "32Gi", LatencyMillis: 12}
			b := newSpokeCluster("beta", "default", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeKubeconfig)
			cli = fakeClientWith(a, b)
		})

		It("json round-trips to the list", func() {
			var buf bytes.Buffer
			Expect(runSpokeClusterList(ctx, cli, "", true, &buf, "json")).To(Succeed())
			var got v1beta1.SpokeClusterList
			Expect(json.Unmarshal(buf.Bytes(), &got)).To(Succeed())
			Expect(got.Items).To(HaveLen(2))
		})

		It("yaml emits the resources", func() {
			var buf bytes.Buffer
			Expect(runSpokeClusterList(ctx, cli, "", true, &buf, "yaml")).To(Succeed())
			Expect(buf.String()).To(ContainSubstring("alpha"))
			Expect(buf.String()).To(ContainSubstring("beta"))
		})

		It("wide appends the discovered columns", func() {
			var buf bytes.Buffer
			Expect(runSpokeClusterList(ctx, cli, "", true, &buf, "wide")).To(Succeed())
			for _, header := range []string{"REGION", "ENDPOINT", "CPU", "MEMORY", "LATENCY", "LAST PROBE"} {
				Expect(buf.String()).To(ContainSubstring(header))
			}
			Expect(buf.String()).To(ContainSubstring("us-west-2"))
		})

		It("name prints spokecluster/<name> per row without a header", func() {
			var buf bytes.Buffer
			Expect(runSpokeClusterList(ctx, cli, "", true, &buf, "name")).To(Succeed())
			Expect(buf.String()).To(ContainSubstring("spokecluster/alpha"))
			Expect(buf.String()).To(ContainSubstring("spokecluster/beta"))
			Expect(buf.String()).ToNot(ContainSubstring("NAME"))
		})

		It("errors on an unknown format, listing the supported ones", func() {
			var buf bytes.Buffer
			err := runSpokeClusterList(ctx, cli, "", true, &buf, "toml")
			Expect(err).To(HaveOccurred())
			for _, f := range []string{"table", "wide", "json", "yaml", "name"} {
				Expect(err.Error()).To(ContainSubstring(f))
			}
		})
	})

	Context("show", func() {
		var cli client.Client
		BeforeEach(func() {
			cli = fakeClientWith(newSpokeCluster("prod", "vela-system", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeAWS))
		})

		It("json round-trips to the resource", func() {
			var buf bytes.Buffer
			Expect(runSpokeClusterShow(ctx, cli, "vela-system", "prod", &buf, "json")).To(Succeed())
			var got v1beta1.SpokeCluster
			Expect(json.Unmarshal(buf.Bytes(), &got)).To(Succeed())
			Expect(got.Name).To(Equal("prod"))
		})

		It("yaml emits the resource", func() {
			var buf bytes.Buffer
			Expect(runSpokeClusterShow(ctx, cli, "vela-system", "prod", &buf, "yaml")).To(Succeed())
			Expect(buf.String()).To(ContainSubstring("prod"))
		})

		It("rejects the name format", func() {
			var buf bytes.Buffer
			err := runSpokeClusterShow(ctx, cli, "vela-system", "prod", &buf, "name")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("table"))
			Expect(err.Error()).To(ContainSubstring("json"))
		})
	})
})

var _ = Describe("vela cluster spokes CRD-absent handling", func() {
	ctx := context.Background()

	It("returns an actionable message for list", func() {
		var buf bytes.Buffer
		err := runSpokeClusterList(ctx, noMatchClient(), "vela-system", false, &buf, "table")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("SpokeCluster CRD"))
		Expect(err.Error()).To(ContainSubstring("vela-core chart"))
		// The gate must not be blamed: it never removes the CRD.
		Expect(err.Error()).NotTo(ContainSubstring("EnableClusterInfrastructure"))
	})

	It("returns an actionable message for show", func() {
		var buf bytes.Buffer
		err := runSpokeClusterShow(ctx, noMatchClient(), "vela-system", "x", &buf, "table")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("SpokeCluster CRD"))
		Expect(err.Error()).To(ContainSubstring("vela-core chart"))
		// The gate must not be blamed: it never removes the CRD.
		Expect(err.Error()).NotTo(ContainSubstring("EnableClusterInfrastructure"))
	})
})

var _ = Describe("vela cluster spokes timeout, latency, and condition age", func() {
	It("surfaces a deadline error rather than blocking", func() {
		// A context whose deadline is already in the past.
		ctx, cancel := context.WithTimeout(context.Background(), -time.Second)
		defer cancel()
		cli := fake.NewClientBuilder().WithScheme(spokeClusterScheme()).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if err := ctx.Err(); err != nil {
						return err
					}
					return c.List(ctx, list, opts...)
				},
			}).Build()

		var buf bytes.Buffer
		err := runSpokeClusterList(ctx, cli, "vela-system", false, &buf, "table")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("deadline exceeded"))
	})

	It("renders zero latency as dash in show and wide list", func() {
		zero := newSpokeCluster("z", "vela-system", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeAWS)
		zero.Status.ClusterInfo = &v1beta1.SpokeClusterInfo{KubernetesVersion: "v1.30", LatencyMillis: 0}

		var showBuf bytes.Buffer
		Expect(runSpokeClusterShow(context.Background(), fakeClientWith(zero), "vela-system", "z", &showBuf, "table")).To(Succeed())
		Expect(showBuf.String()).ToNot(ContainSubstring("0ms"))

		var listBuf bytes.Buffer
		Expect(runSpokeClusterList(context.Background(), fakeClientWith(zero), "vela-system", false, &listBuf, "wide")).To(Succeed())
		Expect(listBuf.String()).ToNot(ContainSubstring("0ms"))
	})

	It("renders condition age, dash when the transition time is unset", func() {
		Expect(conditionAge(metav1.Condition{})).To(Equal("-"))
		recent := metav1.Condition{LastTransitionTime: metav1.NewTime(time.Now().Add(-5 * time.Minute))}
		Expect(conditionAge(recent)).ToNot(Equal("-"))
	})

	It("includes an AGE column in the show conditions table", func() {
		sc := newSpokeCluster("c", "vela-system", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeAWS)
		sc.Status.Conditions = []metav1.Condition{{
			Type: "Connected", Status: metav1.ConditionTrue, Reason: "OK", Message: "up",
			LastTransitionTime: metav1.NewTime(time.Now().Add(-5 * time.Minute)),
		}}
		var buf bytes.Buffer
		Expect(runSpokeClusterShow(context.Background(), fakeClientWith(sc), "vela-system", "c", &buf, "table")).To(Succeed())
		Expect(buf.String()).To(ContainSubstring("AGE"))
		Expect(buf.String()).To(ContainSubstring("5m"))
	})
})

var _ = Describe("vela cluster spokes create", func() {
	ctx := context.Background()

	It("creates a Secret from --kubeconfig and a SpokeCluster", func() {
		dir := GinkgoT().TempDir()
		path := dir + "/spoke.kubeconfig"
		Expect(os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600)).To(Succeed())

		cli := fakeClientWith()
		var buf bytes.Buffer
		Expect(runSpokeClusterCreate(ctx, cli, &buf, spokeClusterCreateOpts{
			Name: "demo", Namespace: "vela-system", KubeconfigPath: path,
		})).To(Succeed())
		Expect(buf.String()).To(ContainSubstring("Created Secret vela-system/demo-kubeconfig"))
		Expect(buf.String()).To(ContainSubstring("Created SpokeCluster vela-system/demo"))

		var secret corev1.Secret
		Expect(cli.Get(ctx, client.ObjectKey{Namespace: "vela-system", Name: "demo-kubeconfig"}, &secret)).To(Succeed())
		Expect(string(secret.Data["kubeconfig"])).To(ContainSubstring("kind: Config"))

		var sc v1beta1.SpokeCluster
		Expect(cli.Get(ctx, client.ObjectKey{Namespace: "vela-system", Name: "demo"}, &sc)).To(Succeed())
		Expect(sc.Spec.Mode).To(Equal(v1beta1.SpokeClusterModeConnect))
		Expect(sc.Spec.Credential.Type).To(Equal(v1beta1.CredentialTypeKubeconfig))
		Expect(sc.Spec.Credential.Kubeconfig.SecretRef.Name).To(Equal("demo-kubeconfig"))
		Expect(sc.Spec.DeletionPolicy).To(Equal(v1beta1.SpokeDeletionPolicyDetach))
	})

	It("reuses an existing Secret when only --secret is set", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: "vela-system"},
			Data:       map[string][]byte{"kubeconfig": []byte("unused")},
		}
		cli := fakeClientWith(secret)
		var buf bytes.Buffer
		Expect(runSpokeClusterCreate(ctx, cli, &buf, spokeClusterCreateOpts{
			Name: "demo", Namespace: "vela-system", SecretName: "existing",
		})).To(Succeed())
		Expect(buf.String()).ToNot(ContainSubstring("Created Secret"))

		var sc v1beta1.SpokeCluster
		Expect(cli.Get(ctx, client.ObjectKey{Namespace: "vela-system", Name: "demo"}, &sc)).To(Succeed())
		Expect(sc.Spec.Credential.Kubeconfig.SecretRef.Name).To(Equal("existing"))
	})

	It("rejects create when the SpokeCluster already exists", func() {
		sc := newSpokeCluster("demo", "vela-system", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeKubeconfig)
		err := runSpokeClusterCreate(ctx, fakeClientWith(sc), &bytes.Buffer{}, spokeClusterCreateOpts{
			Name: "demo", SecretName: "existing",
		})
		Expect(err).To(MatchError(ContainSubstring("already exists")))
	})

	It("rejects create when neither --kubeconfig, --secret, nor --aws is set", func() {
		err := runSpokeClusterCreate(ctx, fakeClientWith(), &bytes.Buffer{}, spokeClusterCreateOpts{Name: "demo"})
		Expect(err).To(MatchError(ContainSubstring("--kubeconfig, --secret, or --aws")))
	})

	It("creates an AWS SpokeCluster with no Secret", func() {
		cli := fakeClientWith()
		var buf bytes.Buffer
		Expect(runSpokeClusterCreate(ctx, cli, &buf, spokeClusterCreateOpts{
			Name: "prod-east", Namespace: "vela-system", AWS: true,
			AWSRegion: "us-west-2", AWSRoleARN: "arn:aws:iam::111122223333:role/spokecluster-prod-east",
			AWSExternalID: "us-west-2/111122223333/hub/vela-system/vela-core-cluster-core",
		})).To(Succeed())
		Expect(buf.String()).ToNot(ContainSubstring("Created Secret"))
		Expect(buf.String()).To(ContainSubstring("Created SpokeCluster vela-system/prod-east"))

		var sc v1beta1.SpokeCluster
		Expect(cli.Get(ctx, client.ObjectKey{Namespace: "vela-system", Name: "prod-east"}, &sc)).To(Succeed())
		Expect(sc.Spec.Credential.Type).To(Equal(v1beta1.CredentialTypeAWS))
		Expect(sc.Spec.Credential.Kubeconfig).To(BeNil())
		Expect(sc.Spec.Credential.AWS).ToNot(BeNil())
		Expect(sc.Spec.Credential.AWS.AuthMode).To(Equal(v1beta1.AWSAuthModePodIdentity))
		Expect(sc.Spec.Credential.AWS.ClusterName).To(Equal("prod-east"))
		Expect(sc.Spec.Credential.AWS.Region).To(Equal("us-west-2"))
		Expect(sc.Spec.Credential.AWS.RoleARN).To(Equal("arn:aws:iam::111122223333:role/spokecluster-prod-east"))
		Expect(sc.Spec.Credential.AWS.ExternalID).To(ContainSubstring("vela-core-cluster-core"))
	})

	It("honors --aws-cluster-name and --aws-auth-mode irsa", func() {
		cli := fakeClientWith()
		Expect(runSpokeClusterCreate(ctx, cli, &bytes.Buffer{}, spokeClusterCreateOpts{
			Name: "alias", Namespace: "vela-system", AWS: true,
			AWSAuthMode: "irsa", AWSClusterName: "prod-west",
			AWSRegion: "us-east-1", AWSRoleARN: "arn:aws:iam::1:role/scoped",
			AWSExternalID: "us-east-1/1/hub/vela-system/vela-core-cluster-core",
		})).To(Succeed())
		var sc v1beta1.SpokeCluster
		Expect(cli.Get(ctx, client.ObjectKey{Namespace: "vela-system", Name: "alias"}, &sc)).To(Succeed())
		Expect(sc.Spec.Credential.AWS.AuthMode).To(Equal(v1beta1.AWSAuthModeIRSA))
		Expect(sc.Spec.Credential.AWS.ClusterName).To(Equal("prod-west"))
	})

	It("rejects --aws mixed with --kubeconfig or --secret", func() {
		err := runSpokeClusterCreate(ctx, fakeClientWith(), &bytes.Buffer{}, spokeClusterCreateOpts{
			Name: "demo", AWS: true, SecretName: "existing",
			AWSRegion: "us-west-2", AWSRoleARN: "arn:aws:iam::1:role/x",
		})
		Expect(err).To(MatchError(ContainSubstring("--aws cannot be used with --kubeconfig or --secret")))
	})

	It("rejects --aws without region or role", func() {
		err := runSpokeClusterCreate(ctx, fakeClientWith(), &bytes.Buffer{}, spokeClusterCreateOpts{
			Name: "demo", AWS: true,
		})
		Expect(err).To(MatchError(ContainSubstring("--aws requires --aws-region and --aws-role-arn")))
	})

	It("rejects --aws without external id", func() {
		err := runSpokeClusterCreate(ctx, fakeClientWith(), &bytes.Buffer{}, spokeClusterCreateOpts{
			Name: "demo", AWS: true,
			AWSRegion: "us-west-2", AWSRoleARN: "arn:aws:iam::1:role/x",
		})
		Expect(err).To(MatchError(ContainSubstring("--aws requires --aws-external-id")))
	})

	It("rejects an unknown --aws-auth-mode", func() {
		err := runSpokeClusterCreate(ctx, fakeClientWith(), &bytes.Buffer{}, spokeClusterCreateOpts{
			Name: "demo", AWS: true, AWSAuthMode: "keys",
			AWSRegion: "us-west-2", AWSRoleARN: "arn:aws:iam::1:role/x",
			AWSExternalID: "ext",
		})
		Expect(err).To(MatchError(ContainSubstring("invalid --aws-auth-mode")))
	})

	It("rejects --kubeconfig when the target Secret already exists", func() {
		dir := GinkgoT().TempDir()
		path := dir + "/spoke.kubeconfig"
		Expect(os.WriteFile(path, []byte("kind: Config\n"), 0o600)).To(Succeed())
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "demo-kubeconfig", Namespace: "vela-system"}}
		err := runSpokeClusterCreate(ctx, fakeClientWith(secret), &bytes.Buffer{}, spokeClusterCreateOpts{
			Name: "demo", KubeconfigPath: path,
		})
		Expect(err).To(MatchError(ContainSubstring("already exists")))
	})

	It("rejects --secret when the Secret is missing", func() {
		err := runSpokeClusterCreate(ctx, fakeClientWith(), &bytes.Buffer{}, spokeClusterCreateOpts{
			Name: "demo", SecretName: "missing",
		})
		Expect(err).To(MatchError(ContainSubstring("not found")))
	})

	It("spokeUsesKubeconfigSecret matches only the named kubeconfig Secret", func() {
		sc := newSpokeCluster("demo", "vela-system", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeKubeconfig)
		Expect(spokeUsesKubeconfigSecret(sc, "demo-kubeconfig")).To(BeFalse())
		sc.Spec.Credential.Kubeconfig = &v1beta1.KubeconfigCredential{
			SecretRef: v1beta1.SecretKeyRef{Name: "demo-kubeconfig"},
		}
		Expect(spokeUsesKubeconfigSecret(sc, "demo-kubeconfig")).To(BeTrue())
		Expect(spokeUsesKubeconfigSecret(sc, "other-kubeconfig")).To(BeFalse())
		Expect(spokeUsesKubeconfigSecret(nil, "demo-kubeconfig")).To(BeFalse())
	})

	It("rejects an unknown deletion policy", func() {
		_, err := parseSpokeDeletionPolicy("wipe")
		Expect(err).To(MatchError(ContainSubstring("invalid --deletion-policy")))
	})

	It("deletes a Secret it just wrote if SpokeCluster create fails", func() {
		dir := GinkgoT().TempDir()
		path := dir + "/spoke.kubeconfig"
		Expect(os.WriteFile(path, []byte("kind: Config\n"), 0o600)).To(Succeed())

		cli := fake.NewClientBuilder().WithScheme(spokeClusterScheme()).
			WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if _, ok := obj.(*v1beta1.SpokeCluster); ok {
						return fmt.Errorf("admission denied")
					}
					return c.Create(ctx, obj, opts...)
				},
			}).Build()

		err := runSpokeClusterCreate(ctx, cli, &bytes.Buffer{}, spokeClusterCreateOpts{
			Name: "demo", Namespace: "vela-system", KubeconfigPath: path,
		})
		Expect(err).To(MatchError(ContainSubstring("admission denied")))
		Expect(cli.Get(ctx, client.ObjectKey{Namespace: "vela-system", Name: "demo-kubeconfig"}, &corev1.Secret{})).
			To(MatchError(ContainSubstring("not found")))
	})

	It("keeps the Secret when create errors but the SpokeCluster exists", func() {
		dir := GinkgoT().TempDir()
		path := dir + "/spoke.kubeconfig"
		Expect(os.WriteFile(path, []byte("kind: Config\n"), 0o600)).To(Succeed())

		cli := fake.NewClientBuilder().WithScheme(spokeClusterScheme()).
			WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if err := c.Create(ctx, obj, opts...); err != nil {
						return err
					}
					if _, ok := obj.(*v1beta1.SpokeCluster); ok {
						return fmt.Errorf("timeout after persist")
					}
					return nil
				},
			}).Build()

		err := runSpokeClusterCreate(ctx, cli, &bytes.Buffer{}, spokeClusterCreateOpts{
			Name: "demo", Namespace: "vela-system", KubeconfigPath: path,
		})
		Expect(err).To(MatchError(ContainSubstring("timeout after persist")))
		Expect(cli.Get(ctx, client.ObjectKey{Namespace: "vela-system", Name: "demo-kubeconfig"}, &corev1.Secret{})).To(Succeed())
		Expect(cli.Get(ctx, client.ObjectKey{Namespace: "vela-system", Name: "demo"}, &v1beta1.SpokeCluster{})).To(Succeed())
	})

	It("reports when leftover Secret cleanup fails", func() {
		dir := GinkgoT().TempDir()
		path := dir + "/spoke.kubeconfig"
		Expect(os.WriteFile(path, []byte("kind: Config\n"), 0o600)).To(Succeed())

		cli := fake.NewClientBuilder().WithScheme(spokeClusterScheme()).
			WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if _, ok := obj.(*v1beta1.SpokeCluster); ok {
						return fmt.Errorf("admission denied")
					}
					return c.Create(ctx, obj, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if _, ok := obj.(*corev1.Secret); ok {
						return fmt.Errorf("delete forbidden")
					}
					return c.Delete(ctx, obj, opts...)
				},
			}).Build()

		err := runSpokeClusterCreate(ctx, cli, &bytes.Buffer{}, spokeClusterCreateOpts{
			Name: "demo", Namespace: "vela-system", KubeconfigPath: path,
		})
		Expect(err).To(MatchError(ContainSubstring("admission denied")))
		Expect(err).To(MatchError(ContainSubstring("leftover Secret")))
		Expect(err).To(MatchError(ContainSubstring("delete forbidden")))
	})

	It("deletes our Secret when create is AlreadyExists for a different Secret", func() {
		dir := GinkgoT().TempDir()
		path := dir + "/spoke.kubeconfig"
		Expect(os.WriteFile(path, []byte("kind: Config\n"), 0o600)).To(Succeed())

		winner := newSpokeCluster("demo", "vela-system", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeKubeconfig)
		winner.Spec.Credential.Kubeconfig = &v1beta1.KubeconfigCredential{
			SecretRef: v1beta1.SecretKeyRef{Name: "winner-kubeconfig"},
		}
		spokeGets := 0
		cli := fake.NewClientBuilder().WithScheme(spokeClusterScheme()).WithObjects(winner).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*v1beta1.SpokeCluster); ok && key.Name == "demo" {
						spokeGets++
						if spokeGets == 1 {
							return apierrors.NewNotFound(schema.GroupResource{Group: "core.oam.dev", Resource: "spokeclusters"}, key.Name)
						}
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if _, isSecret := obj.(*corev1.Secret); !isSecret && obj.GetName() == "demo" {
						return apierrors.NewAlreadyExists(schema.GroupResource{Group: "core.oam.dev", Resource: "spokeclusters"}, obj.GetName())
					}
					return c.Create(ctx, obj, opts...)
				},
			}).Build()

		err := runSpokeClusterCreate(ctx, cli, &bytes.Buffer{}, spokeClusterCreateOpts{
			Name: "demo", Namespace: "vela-system", KubeconfigPath: path, SecretName: "loser-kubeconfig",
		})
		Expect(err).To(MatchError(ContainSubstring("already exists")))
		Expect(cli.Get(ctx, client.ObjectKey{Namespace: "vela-system", Name: "loser-kubeconfig"}, &corev1.Secret{})).
			To(MatchError(ContainSubstring("not found")))
		var sc v1beta1.SpokeCluster
		Expect(cli.Get(ctx, client.ObjectKey{Namespace: "vela-system", Name: "demo"}, &sc)).To(Succeed())
		Expect(sc.Spec.Credential.Kubeconfig.SecretRef.Name).To(Equal("winner-kubeconfig"))
	})

	It("keeps the Secret when create is AlreadyExists after our own persist", func() {
		dir := GinkgoT().TempDir()
		path := dir + "/spoke.kubeconfig"
		Expect(os.WriteFile(path, []byte("kind: Config\n"), 0o600)).To(Succeed())

		cli := fake.NewClientBuilder().WithScheme(spokeClusterScheme()).
			WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if err := c.Create(ctx, obj, opts...); err != nil {
						return err
					}
					if sc, ok := obj.(*v1beta1.SpokeCluster); ok {
						return apierrors.NewAlreadyExists(schema.GroupResource{Group: "core.oam.dev", Resource: "spokeclusters"}, sc.Name)
					}
					return nil
				},
			}).Build()

		err := runSpokeClusterCreate(ctx, cli, &bytes.Buffer{}, spokeClusterCreateOpts{
			Name: "demo", Namespace: "vela-system", KubeconfigPath: path,
		})
		Expect(err).To(MatchError(ContainSubstring("already exists")))
		Expect(cli.Get(ctx, client.ObjectKey{Namespace: "vela-system", Name: "demo-kubeconfig"}, &corev1.Secret{})).To(Succeed())
		Expect(cli.Get(ctx, client.ObjectKey{Namespace: "vela-system", Name: "demo"}, &v1beta1.SpokeCluster{})).To(Succeed())
	})
})

var _ = Describe("vela cluster spokes detach", func() {
	ctx := context.Background()

	It("deletes the SpokeCluster and leaves the source Secret", func() {
		sc := newSpokeCluster("demo", "vela-system", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeKubeconfig)
		sc.Spec.DeletionPolicy = v1beta1.SpokeDeletionPolicyDetach
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "demo-kubeconfig", Namespace: "vela-system"}}
		cli := fakeClientWith(sc, secret)

		var buf bytes.Buffer
		Expect(runSpokeClusterDetach(ctx, cli, "vela-system", "demo", &buf)).To(Succeed())
		Expect(buf.String()).To(ContainSubstring("Deleted SpokeCluster vela-system/demo"))
		Expect(buf.String()).To(ContainSubstring("deletionPolicy=detach"))

		Expect(cli.Get(ctx, client.ObjectKey{Namespace: "vela-system", Name: "demo"}, &v1beta1.SpokeCluster{})).
			To(MatchError(ContainSubstring("not found")))
		Expect(cli.Get(ctx, client.ObjectKey{Namespace: "vela-system", Name: "demo-kubeconfig"}, &corev1.Secret{})).
			To(Succeed())
	})

	It("returns a clear error when the SpokeCluster is missing", func() {
		err := runSpokeClusterDetach(ctx, fakeClientWith(), "vela-system", "missing", &bytes.Buffer{})
		Expect(err).To(MatchError(ContainSubstring("SpokeCluster vela-system/missing not found")))
	})

	// Detaching the wrong namespace is a no-op that looks like a broken CLI, so it gets
	// the same namespace lookup show does.
	It("names the namespace when detaching a spoke that lives elsewhere", func() {
		elsewhere := newSpokeCluster("tenant-spoke", "team-a", v1beta1.SpokeClusterModeConnect, v1beta1.CredentialTypeKubeconfig)
		err := runSpokeClusterDetach(ctx, fakeClientWith(elsewhere), "vela-system", "tenant-spoke", &bytes.Buffer{})
		Expect(err).To(MatchError(ContainSubstring("it is in team-a")))
		Expect(err).To(MatchError(ContainSubstring("-n team-a")))
	})

	It("defaults create and detach namespaces to vela-system", func() {
		Expect(newSpokeClusterCreateCommand(&common.Args{}).Flags().Lookup("namespace").DefValue).To(Equal("vela-system"))
		Expect(newSpokeClusterDetachCommand(&common.Args{}).Flags().Lookup("namespace").DefValue).To(Equal("vela-system"))
	})
})
