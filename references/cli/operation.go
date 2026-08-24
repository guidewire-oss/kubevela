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

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	types2 "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v2alpha1"
	"github.com/oam-dev/kubevela/apis/types"
	"github.com/oam-dev/kubevela/pkg/utils/common"
	cmdutil "github.com/oam-dev/kubevela/pkg/utils/util"
)

// systemDefinitionNamespace is the fallback namespace OperationTemplates are
// also looked up in, mirroring the two-tier resolution used for
// ComponentDefinition et al.
const systemDefinitionNamespace = "vela-system"

// operationPollInterval is how often `vela operation run` polls status.phase
// while waiting for the Operation to reach a terminal phase.
const operationPollInterval = 2 * time.Second

// NewOperationCommand groups the commands for the Operations KEP
// implementation (KEP 2.15).
//
// TODO(KEP 2.15): relies entirely on the caller's own RBAC. There is no
// permission-filtered discovery and no admission control yet -- do not run
// this against a shared cluster.
func NewOperationCommand(c common.Args, order string, ioStreams cmdutil.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "operation",
		Aliases: []string{"op"},
		Short:   "Discover and invoke Operations against a Component.",
		Long: "Discover and invoke Operations against a Component. This command is a " +
			"work-in-progress implementation of the Operations KEP: it has no permission " +
			"model yet, relies entirely on your own RBAC, and should only be used against a " +
			"disposable namespace.",
		Annotations: map[string]string{
			types.TagCommandType:  types.TypeCD,
			types.TagCommandOrder: order,
		},
	}
	cmd.SetOut(ioStreams.Out)
	cmd.AddCommand(
		NewOperationListCommand(c, ioStreams),
		NewOperationRunCommand(c, ioStreams),
		NewOperationStatusCommand(c, ioStreams),
	)
	return cmd
}

// splitComponentRef parses the "<app>/<name>" shorthand shared by
// `vela operation list` and `vela operation run`.
func splitComponentRef(ref string) (app, name string, err error) {
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("--component must be in the form <app>/<name>, got %q", ref)
	}
	return parts[0], parts[1], nil
}

// resolveComponentType looks up the ComponentDefinition type of a named
// component within an Application, straight from spec.components -- the CLI
// trusts the caller's own RBAC and does no further resolution.
func resolveComponentType(ctx context.Context, k8sClient client.Client, ns, appName, compName string) (string, error) {
	app := &v1beta1.Application{}
	if err := k8sClient.Get(ctx, types2.NamespacedName{Namespace: ns, Name: appName}, app); err != nil {
		return "", errors.Wrapf(err, "get application %q", appName)
	}
	for _, comp := range app.Spec.Components {
		if comp.Name == compName {
			return comp.Type, nil
		}
	}
	return "", fmt.Errorf("component %q not found in application %q", compName, appName)
}

// listAllowedOperationTemplates lists OperationTemplates in ns and (if
// different) vela-system, filtered to attach.scope: Component where
// allowedComponentTypes is empty or contains componentType. Templates in ns
// take precedence over a same-named template in vela-system.
func listAllowedOperationTemplates(ctx context.Context, k8sClient client.Client, ns, componentType string) ([]v2alpha1.OperationTemplate, error) {
	seen := map[string]bool{}
	var out []v2alpha1.OperationTemplate
	namespaces := []string{ns}
	if ns != systemDefinitionNamespace {
		namespaces = append(namespaces, systemDefinitionNamespace)
	}
	for _, tmplNS := range namespaces {
		list := &v2alpha1.OperationTemplateList{}
		if err := k8sClient.List(ctx, list, client.InNamespace(tmplNS)); err != nil {
			return nil, errors.Wrapf(err, "list operation templates in namespace %q", tmplNS)
		}
		for _, tmpl := range list.Items {
			if seen[tmpl.Name] {
				continue
			}
			// Mark seen by name now, before filtering: a template shadowed
			// by an unsupported scope or disallowed component type in the
			// higher-precedence namespace must not fall through to a
			// same-named template further down -- `run` still resolves the
			// higher-precedence copy first and would fail on it.
			seen[tmpl.Name] = true
			if tmpl.Spec.Attach.Scope != "" && tmpl.Spec.Attach.Scope != v2alpha1.OperationAttachScopeComponent {
				continue
			}
			allowed := tmpl.Spec.Attach.AllowedComponentTypes
			if len(allowed) > 0 {
				matched := false
				for _, t := range allowed {
					if t == componentType {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			}
			out = append(out, tmpl)
		}
	}
	return out, nil
}

// NewOperationListCommand creates the `vela operation list` command.
func NewOperationListCommand(c common.Args, ioStreams cmdutil.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List OperationTemplates allowed against a component.",
		Long:    "List OperationTemplates in the target namespace and vela-system that can be invoked against a given component.",
		Example: "vela operation list --component myapp/myserver",
		Args:    cobra.ExactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			componentRef, err := cmd.Flags().GetString(FlagComponent)
			if err != nil {
				return err
			}
			ns, err := GetFlagNamespace(cmd, c)
			if err != nil {
				return err
			}
			if ns == "" {
				ns, err = GetNamespaceFromEnv(cmd, c)
				if err != nil {
					return err
				}
			}
			appName, compName, err := splitComponentRef(componentRef)
			if err != nil {
				return err
			}
			k8sClient, err := c.GetClient()
			if err != nil {
				return errors.Wrap(err, "failed to get k8s client")
			}
			componentType, err := resolveComponentType(ctx, k8sClient, ns, appName, compName)
			if err != nil {
				return err
			}
			templates, err := listAllowedOperationTemplates(ctx, k8sClient, ns, componentType)
			if err != nil {
				return err
			}
			if len(templates) == 0 {
				cmd.Println("No operation template found.")
				return nil
			}
			table := newUITable()
			table.AddRow("NAME", "ALLOWED TYPES", "DESCRIPTION")
			for _, tmpl := range templates {
				allowed := "*"
				if len(tmpl.Spec.Attach.AllowedComponentTypes) > 0 {
					allowed = strings.Join(tmpl.Spec.Attach.AllowedComponentTypes, ",")
				}
				table.AddRow(tmpl.Name, allowed, tmpl.Spec.Description)
			}
			cmd.Println(table)
			return nil
		},
	}
	cmd.Flags().StringP(FlagComponent, "c", "", "the target component, as <app>/<name>")
	addNamespaceAndEnvArg(cmd)
	return cmd
}

// NewOperationRunCommand creates the `vela operation run` command.
func NewOperationRunCommand(c common.Args, ioStreams cmdutil.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "run <template>",
		Short:   "Invoke an OperationTemplate against a component.",
		Long:    "Create an Operation from the given OperationTemplate against a target component, then wait for it to finish.",
		Example: "vela operation run restart --component myapp/myserver --param force=true",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			templateName := args[0]
			componentRef, err := cmd.Flags().GetString(FlagComponent)
			if err != nil {
				return err
			}
			ns, err := GetFlagNamespace(cmd, c)
			if err != nil {
				return err
			}
			if ns == "" {
				ns, err = GetNamespaceFromEnv(cmd, c)
				if err != nil {
					return err
				}
			}
			paramFlags, err := cmd.Flags().GetStringArray(FlagParam)
			if err != nil {
				return err
			}
			appName, compName, err := splitComponentRef(componentRef)
			if err != nil {
				return err
			}
			params, err := parseOperationParams(paramFlags)
			if err != nil {
				return err
			}
			k8sClient, err := c.GetClient()
			if err != nil {
				return errors.Wrap(err, "failed to get k8s client")
			}

			op := &v2alpha1.Operation{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: templateName + "-",
					Namespace:    ns,
				},
				Spec: v2alpha1.OperationSpec{
					Template: templateName,
					Target: v2alpha1.OperationTarget{
						App:       appName,
						Component: compName,
					},
				},
			}
			if len(params) > 0 {
				raw, err := json.Marshal(params)
				if err != nil {
					return errors.Wrap(err, "marshal --param values")
				}
				op.Spec.Parameters = &runtime.RawExtension{Raw: raw}
			}
			if err := k8sClient.Create(ctx, op); err != nil {
				return errors.Wrap(err, "create operation")
			}
			cmd.Printf("Operation %q created, waiting for it to finish...\n", op.Name)

			for {
				if err := k8sClient.Get(ctx, types2.NamespacedName{Namespace: op.Namespace, Name: op.Name}, op); err != nil {
					return errors.Wrap(err, "get operation")
				}
				if op.IsTerminal() {
					break
				}
				time.Sleep(operationPollInterval)
			}
			printOperationStatus(cmd, op)
			if op.Status.Phase != v2alpha1.OperationPhaseSucceeded {
				return fmt.Errorf("operation %q did not succeed: %s", op.Name, op.Status.Message)
			}
			return nil
		},
	}
	cmd.Flags().StringP(FlagComponent, "c", "", "the target component, as <app>/<name>")
	cmd.Flags().StringArrayP(FlagParam, "p", nil, "a key=value parameter, may be repeated")
	addNamespaceAndEnvArg(cmd)
	return cmd
}

// NewOperationStatusCommand creates the `vela operation status` command.
func NewOperationStatusCommand(c common.Args, ioStreams cmdutil.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "status <name>",
		Short:   "Show the status of an Operation.",
		Long:    "Fetch and print the current status of an existing Operation.",
		Example: "vela operation status restart-abc123",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			ns, err := GetFlagNamespace(cmd, c)
			if err != nil {
				return err
			}
			if ns == "" {
				ns, err = GetNamespaceFromEnv(cmd, c)
				if err != nil {
					return err
				}
			}
			k8sClient, err := c.GetClient()
			if err != nil {
				return errors.Wrap(err, "failed to get k8s client")
			}
			op := &v2alpha1.Operation{}
			if err := k8sClient.Get(ctx, types2.NamespacedName{Namespace: ns, Name: args[0]}, op); err != nil {
				if kerrors.IsNotFound(err) {
					return fmt.Errorf("operation %q not found in namespace %q", args[0], ns)
				}
				return errors.Wrap(err, "get operation")
			}
			printOperationStatus(cmd, op)
			return nil
		},
	}
	addNamespaceAndEnvArg(cmd)
	return cmd
}

// printOperationStatus is shared by `run`'s polling output and `status`'s
// one-shot fetch, so "watch it finish" and "check on it later" render
// identically.
func printOperationStatus(cmd *cobra.Command, op *v2alpha1.Operation) {
	cmd.Printf("Phase: %s\n", op.Status.Phase)
	if op.Status.Message != "" {
		cmd.Printf("Message: %s\n", op.Status.Message)
	}
	if op.Status.CompletionTime != nil {
		cmd.Printf("Completed: %s\n", op.Status.CompletionTime.Format(time.RFC3339))
	}
	if len(op.Status.Workflows) == 0 || len(op.Status.Workflows[0].Steps) == 0 {
		return
	}
	table := newUITable()
	table.AddRow("STEP", "PHASE", "MESSAGE")
	for _, step := range op.Status.Workflows[0].Steps {
		table.AddRow(step.Name, step.Phase, step.Message)
	}
	cmd.Println(table)
}

// parseOperationParams parses "key=value" flags into a map.
// TODO(KEP 2.15): values are kept as strings -- no schema validation or
// type coercion is performed yet.
func parseOperationParams(flags []string) (map[string]interface{}, error) {
	if len(flags) == 0 {
		return nil, nil
	}
	out := make(map[string]interface{}, len(flags))
	for _, f := range flags {
		parts := strings.SplitN(f, "=", 2)
		if len(parts) != 2 || parts[0] == "" {
			return nil, fmt.Errorf("--param must be in the form key=value, got %q", f)
		}
		out[parts[0]] = parts[1]
	}
	return out, nil
}
