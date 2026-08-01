import "vela/kube"

// The KubeVela environment an Application is deployed into.
//
// An env is a Namespace carrying two labels - `usage.oam.dev/control-plane: env`
// marking it as one, and `namespace.oam.dev/env: <name>` naming it. That is the
// whole mechanism, which is why this needs no Go: it is one `kube.#Get`.
//
// It exists as its own source rather than a generic `namespace` one because the
// environment is the concept a platform team and an application team share. A
// namespace is where it happens to live. Reading `source.env.name` says what it
// means; reading a label key off a namespace says how it is stored, and ties
// every Application to that storage choice.
//
// `name` is optional and deliberately so: a namespace that was never `vela env
// init`-ed carries no env label, so the read may find nothing and a consumer
// must supply a default. That is honest - not every namespace is an environment.
//
// `labels` and `annotations` are open maps, so a key read carries the usual
// default obligation when it feeds a required parameter. This is where platforms
// keep tenancy - team, cost centre, tier - and a definition wanting a typed
// contract over those should name its fields rather than hand out raw keys.
schema: {
	name?:     string
	namespace: string
	labels: [string]:      string
	annotations: [string]: string
}

storage: {
	// A namespace's labels change when an operator changes them. The read is a
	// single GET, but it is on the hot path of every render that consumes it.
	storageTTL:     "5m"
	onStaleFailure: "use-stale"
}

parameter: {
	// +usage=Namespace backing the environment. Defaults to the Application's own.
	namespace?: string
}

_ns: kube.#Get & {
	$params: resource: {
		apiVersion: "v1"
		kind:       "Namespace"
		metadata: name: [
			if parameter.namespace != _|_ {parameter.namespace},
			context.namespace,
		][0]
	}
}

_labels: *_ns.$returns.metadata.labels | {}

output: {
	if _labels["namespace.oam.dev/env"] != _|_ {
		name: _labels["namespace.oam.dev/env"]
	}
	namespace:   _ns.$returns.metadata.name
	labels:      _labels
	annotations: *_ns.$returns.metadata.annotations | {}
}
