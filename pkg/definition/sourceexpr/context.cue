// The context registry: every field KubeVela's render context can carry, its
// type, and which call sites offer it.
//
// This file is the source of truth. Go reads it - it does not restate it. A
// surface's readable set is a CUE type here, so the types an expression is
// checked against at admission and the values it is evaluated against at render
// come from one declaration and cannot drift apart.
//
// Adding a field: put it in the group that describes when it exists, and it
// appears on every surface embedding that group. A field that is present in the
// render context but readable nowhere goes in `excluded` with a reason - the
// drift tests require every field to be in one place or the other, so a new one
// upstream forces a decision rather than being silently unavailable.

// #AppIdentity is what every call site knows: which Application, where, and at
// which revision.
#AppIdentity: {
	// +usage=The Application's name
	appName: string
	// +usage=The namespace the Application is deployed to
	namespace: string
	// +usage=Name of the current ApplicationRevision; empty before the first exists
	appRevision: string
	// +usage=Revision ordinal; 0 before the first revision exists
	appRevisionNum: int
	// +usage=Labels on the Application, read by key
	appLabels: [string]: string
	// +usage=Annotations on the Application, read by key
	appAnnotations: [string]: string
}

// #DeliveryIdentity is what a render targeting a cluster knows. Absent from
// paths that run before placement is decided.
#DeliveryIdentity: {
	// +usage=The cluster being rendered for
	cluster: string
	// +usage=Version of the target cluster. A struct, so never a cache-key segment
	clusterVersion: {
		major:      string
		minor:      int
		gitVersion: string
		platform:   string
	}
	// +usage=The publish version, when set
	publishVersion: string
	// +usage=The workflow being executed, when set
	workflowName: string
}

// #ComponentIdentity is what a component or trait render knows about the
// component it is rendering.
#ComponentIdentity: {
	// +usage=Name of the component revision
	revision: string
	// +usage=Replica key, set by the replication policy
	replicaKey: string
}

// #PolicyIdentity is the policy instance being rendered.
#PolicyIdentity: {
	// +usage=The policy's name, from its spec.policies[] entry
	policyName: string
	// +usage=The policy's type
	policyType: string
}

// #PolicyRevisionIdentity is version metadata, available only where a policy
// renders through renderPolicyCUETemplate.
#PolicyRevisionIdentity: {
	policyRevisionName: string
	policyRevisionHash: string
	policyRevision:     int
}

// #PublishedContext is what an Application-scoped policy published via output.ctx,
// wrapped under `custom` by NewContext and carried into every render that follows.
//
// Two things make it unlike every other field. It is absent unless some policy
// set it, and its shape is whatever that policy chose - so it cannot be typed
// beyond `_`. Both are already handled rules rather than special cases: a read
// into an open region must assert its type (`& string`), and a read that may be
// absent must carry a default (`*... | fallback`). Using it needs both.
#PublishedContext: {
	// +usage=Data published by an Application-scoped policy's output.ctx. Absent
	// unless a policy set it, and unshaped - a read needs a type assertion and a default
	custom: _
}

// labels name each surface in error messages, so the message says "application-
// scoped policy" rather than the key used to look it up.
labels: {
	component:        "component"
	trait:            "trait"
	workflowstep:     "workflow step"
	"policy-default": "policy"
	"policy-app":     "application-scoped policy"
}

// surfaces are the call sites. Each is the readable context at that point.
//
// `name` is declared per surface rather than in a group because it genuinely
// means something different at each: the component, the Application, the binding.
// That ambiguity is why the policy surfaces omit it and offer policyName instead.
surfaces: {
	// A ComponentDefinition or TraitDefinition template, and the property
	// expressions substituted immediately before it runs.
	component: {
		#AppIdentity
		#DeliveryIdentity
		#ComponentIdentity
		#PublishedContext
		// +usage=The component being rendered
		name: string
	}
	trait: surfaces.component

	// A workflow step's properties, substituted before the engine sees them.
	workflowstep: {
		#AppIdentity
		#DeliveryIdentity
		#PublishedContext
		// +usage=The step being rendered
		name: string
	}

	// A policy whose expressions are substituted while the appfile is built,
	// before any render. Narrower than the scoped path because it draws on the
	// Appfile alone - there is no cluster yet, and no policy revision metadata.
	"policy-default": {
		#AppIdentity
		#PolicyIdentity
	}

	// An Application-scoped policy, rendered by renderPolicyCUETemplate. It gets
	// revision metadata and clusterVersion, but no cluster: that render targets
	// no cluster at all.
	"policy-app": {
		#AppIdentity
		#PolicyIdentity
		#PolicyRevisionIdentity
		// A later scoped policy sees what an earlier one published:
		// storeAdditionalContextInCtx merges rather than replaces.
		#PublishedContext
		clusterVersion: {
			major:      string
			minor:      int
			gitVersion: string
			platform:   string
		}
	}
}

// excluded are fields the render context carries that *no* surface offers, each
// with the reason. Being explicit is what makes the drift tests useful.
//
// A field available on some surfaces but not others does not belong here: the
// reason is derivable, and reporting "available on: component, trait" beats prose
// that has to be kept true by hand.
excluded: {
	// +reason=internal plumbing for source resolution, not user-facing context
	appSources: _
	// +reason=internal plumbing for source resolution, not user-facing context
	appSourceTypes: _
	// +reason=internal plumbing for source resolution, not user-facing context
	appSourceTemplates: _
	// +reason=internal plumbing for source resolution, not user-facing context
	appSourceSensitivePaths: _
	// +reason=internal plumbing for source resolution, not user-facing context
	appSourceCacheStore: _
	// +reason=internal plumbing for source resolution, not user-facing context
	sourceResolutionStatuses: _
	// +reason=an app-wide list; readable in principle but not yet typed here
	components: _
	// +reason=an app-wide list; readable in principle but not yet typed here
	appComponents: _
	// +reason=an app-wide list; readable in principle but not yet typed here
	appPolicies: _
	// +reason=an app-wide object; readable in principle but not yet typed here
	appWorkflow: _
	// +reason=produced by the render, so it does not exist when properties are substituted
	output: _
	// +reason=produced by the render, so it does not exist when properties are substituted
	outputs: _
	// +reason=produced by the render, so it does not exist when properties are substituted
	outputSecretName: _
	// +reason=the properties being substituted; reading them from within is circular
	parameter: _
}
