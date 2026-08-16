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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apitypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/duration"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/utils/common"
)

// Supported values for the -o/--output flag.
const (
	outputTable = "table"
	outputWide  = "wide"
	outputJSON  = "json"
	outputYAML  = "yaml"
	outputName  = "name"

	listOutputFormats = "table, wide, json, yaml, name"
	showOutputFormats = "table, wide, json, yaml"
)

const dash = "-"

// defaultSpokeClusterNamespace is where SpokeClusters normally live; show defaults here.
const defaultSpokeClusterNamespace = "vela-system"

// NewSpokeClusterCommandGroup creates the `vela cluster spokes` command group for
// managing SpokeCluster resources reconciled by vela-cluster-core.
func NewSpokeClusterCommandGroup(c *common.Args) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "spokes",
		Aliases: []string{"spoke", "spokecluster", "spokeclusters"},
		Short:   "Manage SpokeCluster resources (Cluster Connect).",
		Long: "Create, inspect, and detach SpokeCluster resources reconciled by vela-cluster-core. " +
			"Requires the EnableClusterInfrastructure feature gate on vela-cluster-core. " +
			"Prefer this over `vela cluster join` for new clusters.",
		// Override the parent `vela cluster` PersistentPreRunE, which resolves the
		// cluster-gateway service and fails when it is absent. cobra runs only the
		// nearest PersistentPreRunE, so this scopes the override to the spokes group:
		// SpokeCluster commands talk to the hub API server directly and must not
		// depend on cluster-gateway. Requirement 8.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
	}
	cmd.AddCommand(
		newSpokeClusterListCommand(c),
		newSpokeClusterShowCommand(c),
		newSpokeClusterCreateCommand(c),
		newSpokeClusterDetachCommand(c),
	)
	return cmd
}

// newSpokeClusterListCommand lists SpokeCluster resources across namespaces.
func newSpokeClusterListCommand(c *common.Args) *cobra.Command {
	var namespace, output string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List SpokeCluster resources.",
		Long:    "List SpokeCluster resources across all namespaces (or one namespace with -n).",
		Args:    cobra.ExactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			k8sClient, err := c.GetClient()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			return runSpokeClusterList(ctx, k8sClient, namespace, cmd.OutOrStdout(), output)
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "",
		"Namespace to list SpokeClusters from. Defaults to all namespaces.")
	cmd.Flags().StringVarP(&output, "output", "o", outputTable,
		"Output format. One of: "+listOutputFormats+".")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Second,
		"Deadline for the API request.")
	return cmd
}

// runSpokeClusterList lists SpokeClusters (all namespaces when namespace is empty)
// and renders them to out in the requested format.
func runSpokeClusterList(ctx context.Context, k8sClient client.Client, namespace string, out io.Writer, output string) error {
	if err := validateFormat(output, listOutputFormats, true); err != nil {
		return err
	}
	var list v1beta1.SpokeClusterList
	var opts []client.ListOption
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := k8sClient.List(ctx, &list, opts...); err != nil {
		return spokeClusterAPIError(err, "failed to list spokeclusters")
	}

	switch output {
	case outputJSON, outputYAML:
		return writeObjectAs(k8sClient.Scheme(), &list, output, out)
	default: // table, wide, name
	}
	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[i].Name < list.Items[j].Name
	})
	if output == outputName {
		for i := range list.Items {
			fmt.Fprintf(out, "spokecluster/%s\n", list.Items[i].Name)
		}
		return nil
	}
	if len(list.Items) == 0 {
		fmt.Fprintln(out, "No SpokeCluster found.")
		return nil
	}
	renderSpokeClusterTable(&list, out, output == outputWide)
	return nil
}

// renderSpokeClusterTable prints the default (and, when wide, extended) table.
func renderSpokeClusterTable(list *v1beta1.SpokeClusterList, out io.Writer, wide bool) {
	headers := []any{"NAME", "NAMESPACE", "MODE", "AUTH", "VERSION", "NODES", "PLATFORM", "STATUS"}
	if wide {
		headers = append(headers, "REGION", "ENDPOINT", "CPU", "MEMORY", "LATENCY", "LAST PROBE")
	}
	table := newUITable().AddRow(headers...)
	for i := range list.Items {
		sc := &list.Items[i]
		row := []any{
			sc.Name,
			sc.Namespace,
			string(sc.Spec.Mode),
			string(sc.Spec.Credential.Type),
			clusterInfoField(sc, func(ci *v1beta1.SpokeClusterInfo) string { return ci.KubernetesVersion }),
			nodesField(sc),
			clusterInfoField(sc, func(ci *v1beta1.SpokeClusterInfo) string { return ci.Platform }),
			connectionString(sc),
		}
		if wide {
			row = append(row,
				clusterInfoField(sc, func(ci *v1beta1.SpokeClusterInfo) string { return ci.Region }),
				clusterInfoField(sc, func(ci *v1beta1.SpokeClusterInfo) string { return ci.APIServerEndpoint }),
				clusterInfoField(sc, func(ci *v1beta1.SpokeClusterInfo) string { return ci.TotalCPU }),
				clusterInfoField(sc, func(ci *v1beta1.SpokeClusterInfo) string { return ci.TotalMemory }),
				latencyField(sc),
				lastProbeAge(sc),
			)
		}
		table.AddRow(row...)
	}
	fmt.Fprintln(out, table.String())
}

// spokeClusterAPIError maps a list/get error to an actionable message. When the API server
// reports no matching kind for SpokeCluster, it says how to install the CRD instead of
// surfacing the raw "no matches for kind" error. Otherwise it wraps err with wrapMsg.
// Requirement 4.
//
// The advice is deliberately about installing the chart, not about the
// EnableClusterInfrastructure feature gate: Helm applies everything under the chart's crds/
// directory unconditionally, so the gate never removes the CRD. A no-match error means the
// chart (or the CRD) was never installed. The gate being off presents differently: the CRD
// resolves and objects can be created, they just never get a status.
func spokeClusterAPIError(err error, wrapMsg string) error {
	if meta.IsNoMatchError(err) {
		return errors.New("the SpokeCluster CRD is not installed; " +
			"install or upgrade the vela-core chart, which ships it under crds/")
	}
	return errors.Wrap(err, wrapMsg)
}

// validateFormat checks output against the supported formats. name is only valid when
// allowName is true (list supports it, show does not). Requirement 7.5.
func validateFormat(output, supported string, allowName bool) error {
	switch output {
	case outputTable, outputWide, outputJSON, outputYAML:
		return nil
	case outputName:
		if allowName {
			return nil
		}
	}
	return errors.Errorf("unsupported output format %q; supported: %s", output, supported)
}

// writeObjectAs marshals a raw API object to out as JSON or YAML, setting its
// GroupVersionKind from the scheme so the output carries apiVersion/kind. Requirement 7.3.
func writeObjectAs(scheme *runtime.Scheme, obj runtime.Object, output string, out io.Writer) error {
	if gvks, _, err := scheme.ObjectKinds(obj); err == nil && len(gvks) > 0 {
		obj.GetObjectKind().SetGroupVersionKind(gvks[0])
	}
	if output == outputYAML {
		data, err := yaml.Marshal(obj)
		if err != nil {
			return errors.Wrap(err, "failed to marshal spokecluster to yaml")
		}
		fmt.Fprint(out, string(data))
		return nil
	}
	data, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return errors.Wrap(err, "failed to marshal spokecluster to json")
	}
	fmt.Fprintln(out, string(data))
	return nil
}

// latencyField renders the last observed hub-to-spoke latency in milliseconds,
// treating zero as not-yet-discovered ("-") rather than a misleading "0ms".
func latencyField(sc *v1beta1.SpokeCluster) string {
	if sc.Status.ClusterInfo == nil || sc.Status.ClusterInfo.LatencyMillis == 0 {
		return dash
	}
	return fmt.Sprintf("%dms", sc.Status.ClusterInfo.LatencyMillis)
}

// conditionAge renders the age since a condition's lastTransitionTime, "-" when unset.
func conditionAge(cond metav1.Condition) string {
	if cond.LastTransitionTime.IsZero() {
		return dash
	}
	return duration.HumanDuration(time.Since(cond.LastTransitionTime.Time))
}

// lastProbeAge renders the age since the last probe, "-" when unset.
func lastProbeAge(sc *v1beta1.SpokeCluster) string {
	if sc.Status.LastProbeTime == nil || sc.Status.LastProbeTime.IsZero() {
		return dash
	}
	return duration.HumanDuration(time.Since(sc.Status.LastProbeTime.Time))
}

// connectionString renders the observed connectivity state, defaulting to Unknown.
func connectionString(sc *v1beta1.SpokeCluster) string {
	if sc.Status.Connection == "" {
		return string(v1beta1.ConnectionStateUnknown)
	}
	return string(sc.Status.Connection)
}

// clusterInfoField renders a string field from status.clusterInfo, or "-" when the
// info block is absent or the field is empty.
func clusterInfoField(sc *v1beta1.SpokeCluster, get func(*v1beta1.SpokeClusterInfo) string) string {
	if sc.Status.ClusterInfo == nil {
		return dash
	}
	if v := get(sc.Status.ClusterInfo); v != "" {
		return v
	}
	return dash
}

// nodesField renders the node count, treating zero as not-yet-discovered ("-").
func nodesField(sc *v1beta1.SpokeCluster) string {
	if sc.Status.ClusterInfo == nil || sc.Status.ClusterInfo.NodeCount == 0 {
		return dash
	}
	return fmt.Sprintf("%d", sc.Status.ClusterInfo.NodeCount)
}

// newSpokeClusterShowCommand shows the detail of a single SpokeCluster resource.
func newSpokeClusterShowCommand(c *common.Args) *cobra.Command {
	var namespace, output string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show the detail of a SpokeCluster resource.",
		Long:  "Show the spec summary, discovered cluster info, and conditions of a SpokeCluster.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			k8sClient, err := c.GetClient()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			return runSpokeClusterShow(ctx, k8sClient, namespace, args[0], cmd.OutOrStdout(), output)
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", defaultSpokeClusterNamespace,
		"Namespace of the SpokeCluster.")
	cmd.Flags().StringVarP(&output, "output", "o", outputTable,
		"Output format. One of: "+showOutputFormats+".")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Second,
		"Deadline for the API request.")
	return cmd
}

// runSpokeClusterShow fetches one SpokeCluster and renders its detail to out in the
// requested format.
func runSpokeClusterShow(ctx context.Context, k8sClient client.Client, namespace, name string, out io.Writer, output string) error {
	if err := validateFormat(output, showOutputFormats, false); err != nil {
		return err
	}
	var sc v1beta1.SpokeCluster
	key := apitypes.NamespacedName{Namespace: namespace, Name: name}
	if err := k8sClient.Get(ctx, key, &sc); err != nil {
		if apierrors.IsNotFound(err) {
			return errors.Errorf("SpokeCluster %s/%s not found", namespace, name)
		}
		return spokeClusterAPIError(err, "failed to get spokecluster")
	}
	if output == outputJSON || output == outputYAML {
		return writeObjectAs(k8sClient.Scheme(), &sc, output, out)
	}
	printSpokeCluster(&sc, out)
	return nil
}

// printSpokeCluster renders the spec summary, an optional Cluster Info block, and an
// optional Conditions table for a single SpokeCluster.
func printSpokeCluster(sc *v1beta1.SpokeCluster, out io.Writer) {
	fmt.Fprintf(out, "Name:       %s\n", sc.Name)
	fmt.Fprintf(out, "Namespace:  %s\n", sc.Namespace)
	fmt.Fprintf(out, "Mode:       %s\n", string(sc.Spec.Mode))
	fmt.Fprintf(out, "Auth:       %s\n", string(sc.Spec.Credential.Type))
	fmt.Fprintf(out, "Connection: %s\n", connectionString(sc))

	if ci := sc.Status.ClusterInfo; ci != nil {
		fmt.Fprintln(out, "\nCluster Info:")
		fmt.Fprintf(out, "  Kubernetes Version: %s\n", orDash(ci.KubernetesVersion))
		fmt.Fprintf(out, "  Platform:           %s\n", orDash(ci.Platform))
		fmt.Fprintf(out, "  Region:             %s\n", orDash(ci.Region))
		fmt.Fprintf(out, "  Nodes:              %s\n", nodesField(sc))
		fmt.Fprintf(out, "  CPU:                %s\n", orDash(ci.TotalCPU))
		fmt.Fprintf(out, "  Memory:             %s\n", orDash(ci.TotalMemory))
		fmt.Fprintf(out, "  Endpoint:           %s\n", orDash(ci.APIServerEndpoint))
		fmt.Fprintf(out, "  Latency:            %s\n", latencyField(sc))
	}

	if len(sc.Status.Conditions) > 0 {
		fmt.Fprintln(out, "\nConditions:")
		table := newUITable().AddRow("TYPE", "STATUS", "REASON", "MESSAGE", "AGE")
		for _, cond := range sc.Status.Conditions {
			table.AddRow(cond.Type, string(cond.Status), cond.Reason, cond.Message, conditionAge(cond))
		}
		fmt.Fprintln(out, table.String())
	}
}

// orDash returns s, or "-" when s is empty.
func orDash(s string) string {
	if s == "" {
		return dash
	}
	return s
}

const defaultKubeconfigSecretKey = "kubeconfig"

// spokeClusterCreateOpts is the input for runSpokeClusterCreate.
type spokeClusterCreateOpts struct {
	Name           string
	Namespace      string
	KubeconfigPath string
	SecretName     string
	DeletionPolicy v1beta1.SpokeDeletionPolicy
	AWS            bool
	AWSAuthMode    string
	AWSClusterName string
	AWSRegion      string
	AWSRoleARN     string
	AWSExternalID  string
}

// newSpokeClusterCreateCommand creates a SpokeCluster from a kubeconfig file,
// an existing Secret, or AWS cloud-native identity.
func newSpokeClusterCreateCommand(c *common.Args) *cobra.Command {
	var kubeconfigPath, secretName, deletionPolicy, namespace string
	var awsAuthMode, awsClusterName, awsRegion, awsRoleARN, awsExternalID string
	var aws bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a SpokeCluster from a kubeconfig, an existing Secret, or AWS.",
		Long: "Create a SpokeCluster in connect mode. With --kubeconfig, writes a Secret " +
			"unless --secret names an existing one. With --aws, writes no Secret; the " +
			"controller assumes --aws-role-arn and mints an EKS token.",
		Example: "# Create from a kubeconfig file (writes Secret <name>-kubeconfig)\n" +
			"> vela cluster spokes create my-spoke --kubeconfig ./spoke.kubeconfig\n" +
			"# Point at an existing Secret in the same namespace\n" +
			"> vela cluster spokes create my-spoke --secret my-kubeconfig\n" +
			"# EKS via Pod Identity (no Secret). clusterName defaults to <name>.\n" +
			"> vela cluster spokes create prod-east --aws --aws-region us-west-2 \\\n" +
			"    --aws-role-arn arn:aws:iam::111122223333:role/spokecluster-prod-east",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			policy, err := parseSpokeDeletionPolicy(deletionPolicy)
			if err != nil {
				return err
			}
			k8sClient, err := c.GetClient()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			return runSpokeClusterCreate(ctx, k8sClient, cmd.OutOrStdout(), spokeClusterCreateOpts{
				Name:           args[0],
				Namespace:      namespace,
				KubeconfigPath: kubeconfigPath,
				SecretName:     secretName,
				DeletionPolicy: policy,
				AWS:            aws,
				AWSAuthMode:    awsAuthMode,
				AWSClusterName: awsClusterName,
				AWSRegion:      awsRegion,
				AWSRoleARN:     awsRoleARN,
				AWSExternalID:  awsExternalID,
			})
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", defaultSpokeClusterNamespace,
		"Namespace for the SpokeCluster and its credential Secret.")
	cmd.Flags().StringVar(&kubeconfigPath, "kubeconfig", "",
		"Path to a kubeconfig file. Creates a Secret from it unless --secret names an existing one.")
	cmd.Flags().StringVar(&secretName, "secret", "",
		"Existing Secret name, or the name to write when --kubeconfig is also set. Defaults to <name>-kubeconfig.")
	cmd.Flags().BoolVar(&aws, "aws", false,
		"Create an AWS/EKS SpokeCluster. No Secret is written. Requires --aws-region, --aws-role-arn, and --aws-external-id.")
	cmd.Flags().StringVar(&awsRegion, "aws-region", "",
		"EKS region. Required with --aws.")
	cmd.Flags().StringVar(&awsRoleARN, "aws-role-arn", "",
		"IAM role the hub assumes to reach the spoke. Required with --aws.")
	cmd.Flags().StringVar(&awsClusterName, "aws-cluster-name", "",
		"EKS cluster name. Defaults to the SpokeCluster name.")
	cmd.Flags().StringVar(&awsAuthMode, "aws-auth-mode", string(v1beta1.AWSAuthModePodIdentity),
		"AWS auth mode. One of: podIdentity, irsa.")
	cmd.Flags().StringVar(&awsExternalID, "aws-external-id", "",
		"STS external ID for the role assumption. Required with --aws; must match the role trust policy Condition.")
	cmd.Flags().StringVar(&deletionPolicy, "deletion-policy", string(v1beta1.SpokeDeletionPolicyDetach),
		"Fate of the gateway registration on delete. One of: detach, orphan.")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Second,
		"Deadline for the API request.")
	return cmd
}

func parseSpokeDeletionPolicy(s string) (v1beta1.SpokeDeletionPolicy, error) {
	switch v1beta1.SpokeDeletionPolicy(s) {
	case v1beta1.SpokeDeletionPolicyDetach, v1beta1.SpokeDeletionPolicyOrphan:
		return v1beta1.SpokeDeletionPolicy(s), nil
	default:
		return "", errors.Errorf("invalid --deletion-policy %q (want detach or orphan)", s)
	}
}

// runSpokeClusterCreate writes an optional kubeconfig Secret and a SpokeCluster.
func runSpokeClusterCreate(ctx context.Context, k8sClient client.Client, out io.Writer, opts spokeClusterCreateOpts) error {
	if opts.Namespace == "" {
		opts.Namespace = defaultSpokeClusterNamespace
	}
	if opts.DeletionPolicy == "" {
		opts.DeletionPolicy = v1beta1.SpokeDeletionPolicyDetach
	}

	cred, err := buildCreateCredential(opts)
	if err != nil {
		return err
	}

	var existing v1beta1.SpokeCluster
	err = k8sClient.Get(ctx, apitypes.NamespacedName{Namespace: opts.Namespace, Name: opts.Name}, &existing)
	if err == nil {
		return errors.Errorf("SpokeCluster %s/%s already exists", opts.Namespace, opts.Name)
	}
	if !apierrors.IsNotFound(err) {
		return spokeClusterAPIError(err, "failed to get spokecluster")
	}

	var createdSecret string
	if cred.Type == v1beta1.CredentialTypeKubeconfig {
		secretName, created, err := ensureKubeconfigSecret(ctx, k8sClient, out, opts)
		if err != nil {
			return err
		}
		if created {
			createdSecret = secretName
		}
		cred.Kubeconfig = &v1beta1.KubeconfigCredential{
			SecretRef: v1beta1.SecretKeyRef{Name: secretName},
		}
	}

	sc := &v1beta1.SpokeCluster{
		ObjectMeta: metav1.ObjectMeta{Name: opts.Name, Namespace: opts.Namespace},
		Spec: v1beta1.SpokeClusterSpec{
			Mode:           v1beta1.SpokeClusterModeConnect,
			DeletionPolicy: opts.DeletionPolicy,
			Credential:     cred,
		},
	}
	if err := k8sClient.Create(ctx, sc); err != nil {
		createErr := spokeClusterAPIError(err, "failed to create spokecluster")
		if createdSecret == "" {
			return createErr
		}
		var existing v1beta1.SpokeCluster
		getErr := k8sClient.Get(ctx, apitypes.NamespacedName{Namespace: opts.Namespace, Name: opts.Name}, &existing)
		switch {
		case getErr == nil && (!apierrors.IsAlreadyExists(err) || spokeUsesKubeconfigSecret(&existing, createdSecret)):
			// Timeout after persist, or AlreadyExists from a retried Create of
			// our own object: keep the Secret the SpokeCluster still references.
			return createErr
		case getErr != nil && !apierrors.IsNotFound(getErr):
			return errors.Errorf("%v; could not confirm SpokeCluster %s/%s was absent, Secret %s/%s was left in place: %v",
				createErr, opts.Namespace, opts.Name, opts.Namespace, createdSecret, getErr)
		}
		// Confirmed absent, or AlreadyExists for a different Secret (lost race).
		delErr := k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: createdSecret, Namespace: opts.Namespace,
		}})
		if delErr != nil && !apierrors.IsNotFound(delErr) {
			return errors.Errorf("%v; leftover Secret %s/%s could not be deleted: %v",
				createErr, opts.Namespace, createdSecret, delErr)
		}
		return createErr
	}
	fmt.Fprintf(out, "Created SpokeCluster %s/%s. Watch status with: vela cluster spokes show %s -n %s\n",
		opts.Namespace, opts.Name, opts.Name, opts.Namespace)
	return nil
}

// buildCreateCredential picks the credential arm from create flags.
func buildCreateCredential(opts spokeClusterCreateOpts) (v1beta1.CredentialSpec, error) {
	kubeconfigMode := opts.KubeconfigPath != "" || opts.SecretName != ""
	if opts.AWS && kubeconfigMode {
		return v1beta1.CredentialSpec{}, errors.New("--aws cannot be used with --kubeconfig or --secret")
	}
	if !opts.AWS && !kubeconfigMode {
		return v1beta1.CredentialSpec{}, errors.New("provide --kubeconfig, --secret, or --aws")
	}
	if !opts.AWS {
		return v1beta1.CredentialSpec{Type: v1beta1.CredentialTypeKubeconfig}, nil
	}
	if opts.AWSRegion == "" || opts.AWSRoleARN == "" {
		return v1beta1.CredentialSpec{}, errors.New("--aws requires --aws-region and --aws-role-arn")
	}
	if opts.AWSExternalID == "" {
		return v1beta1.CredentialSpec{}, errors.New("--aws requires --aws-external-id (must match the role trust policy Condition)")
	}
	authMode := v1beta1.AWSAuthMode(opts.AWSAuthMode)
	if authMode == "" {
		authMode = v1beta1.AWSAuthModePodIdentity
	}
	if authMode != v1beta1.AWSAuthModePodIdentity && authMode != v1beta1.AWSAuthModeIRSA {
		return v1beta1.CredentialSpec{}, errors.Errorf("invalid --aws-auth-mode %q (want podIdentity or irsa)", opts.AWSAuthMode)
	}
	clusterName := opts.AWSClusterName
	if clusterName == "" {
		clusterName = opts.Name
	}
	return v1beta1.CredentialSpec{
		Type: v1beta1.CredentialTypeAWS,
		AWS: &v1beta1.AWSCredential{
			AuthMode:    authMode,
			ClusterName: clusterName,
			Region:      opts.AWSRegion,
			RoleARN:     opts.AWSRoleARN,
			ExternalID:  opts.AWSExternalID,
		},
	}, nil
}

// spokeUsesKubeconfigSecret reports whether sc's kubeconfig credential names secretName.
func spokeUsesKubeconfigSecret(sc *v1beta1.SpokeCluster, secretName string) bool {
	if sc == nil || sc.Spec.Credential.Kubeconfig == nil {
		return false
	}
	return sc.Spec.Credential.Kubeconfig.SecretRef.Name == secretName
}

// ensureKubeconfigSecret creates a Secret from --kubeconfig, or checks --secret exists.
// created is true only when this call wrote a new Secret, so the caller can
// delete it if the SpokeCluster create is then rejected.
func ensureKubeconfigSecret(ctx context.Context, k8sClient client.Client, out io.Writer, opts spokeClusterCreateOpts) (string, bool, error) {
	secretName := opts.SecretName
	if opts.KubeconfigPath != "" {
		if secretName == "" {
			secretName = opts.Name + "-kubeconfig"
		}
		data, err := os.ReadFile(opts.KubeconfigPath)
		if err != nil {
			return "", false, errors.Wrap(err, "failed to read kubeconfig")
		}
		if len(data) == 0 {
			return "", false, errors.New("kubeconfig file is empty")
		}
		var secret corev1.Secret
		err = k8sClient.Get(ctx, apitypes.NamespacedName{Namespace: opts.Namespace, Name: secretName}, &secret)
		if err == nil {
			return "", false, errors.Errorf("Secret %s/%s already exists; pass --secret %s without --kubeconfig to reuse it",
				opts.Namespace, secretName, secretName)
		}
		if !apierrors.IsNotFound(err) {
			return "", false, errors.Wrap(err, "failed to get secret")
		}
		secret = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: opts.Namespace},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{defaultKubeconfigSecretKey: data},
		}
		if err := k8sClient.Create(ctx, &secret); err != nil {
			return "", false, errors.Wrap(err, "failed to create kubeconfig secret")
		}
		fmt.Fprintf(out, "Created Secret %s/%s.\n", opts.Namespace, secretName)
		return secretName, true, nil
	}
	var secret corev1.Secret
	err := k8sClient.Get(ctx, apitypes.NamespacedName{Namespace: opts.Namespace, Name: secretName}, &secret)
	if apierrors.IsNotFound(err) {
		return "", false, errors.Errorf("Secret %s/%s not found", opts.Namespace, secretName)
	}
	if err != nil {
		return "", false, errors.Wrap(err, "failed to get secret")
	}
	return secretName, false, nil
}

// newSpokeClusterDetachCommand deletes a SpokeCluster. The controller then
// detaches or orphans the gateway registration per deletionPolicy.
func newSpokeClusterDetachCommand(c *common.Args) *cobra.Command {
	var namespace string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "detach <name>",
		Short: "Delete a SpokeCluster (controller detaches or orphans the registration).",
		Long: "Delete the SpokeCluster resource. The controller removes the gateway Secret " +
			"when deletionPolicy is detach (the default), or leaves it when orphan. " +
			"The source kubeconfig Secret is left in place.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			k8sClient, err := c.GetClient()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			return runSpokeClusterDetach(ctx, k8sClient, namespace, args[0], cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", defaultSpokeClusterNamespace,
		"Namespace of the SpokeCluster.")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Second,
		"Deadline for the API request.")
	return cmd
}

// runSpokeClusterDetach deletes one SpokeCluster. The source Secret is not deleted.
func runSpokeClusterDetach(ctx context.Context, k8sClient client.Client, namespace, name string, out io.Writer) error {
	if namespace == "" {
		namespace = defaultSpokeClusterNamespace
	}
	var sc v1beta1.SpokeCluster
	if err := k8sClient.Get(ctx, apitypes.NamespacedName{Namespace: namespace, Name: name}, &sc); err != nil {
		if apierrors.IsNotFound(err) {
			return errors.Errorf("SpokeCluster %s/%s not found", namespace, name)
		}
		return spokeClusterAPIError(err, "failed to get spokecluster")
	}
	if err := k8sClient.Delete(ctx, &sc); err != nil {
		return errors.Wrap(err, "failed to delete spokecluster")
	}
	policy := sc.Spec.DeletionPolicy
	if policy == "" {
		policy = v1beta1.SpokeDeletionPolicyDetach
	}
	msg := fmt.Sprintf("Deleted SpokeCluster %s/%s (deletionPolicy=%s).", namespace, name, policy)
	if sc.Spec.Credential.Type == v1beta1.CredentialTypeKubeconfig {
		msg += " Source Secret is left in place."
	}
	fmt.Fprintln(out, msg)
	return nil
}
