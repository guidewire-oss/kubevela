import "vela/kube"

"vela-app": {
	type: "source"
	annotations: {}
	labels: {}
	description: "The status of another KubeVela Application - phase, overall health, workflow state, and per-component status broken down by the cluster each is placed in. Status only; never its spec."
}

template: {
	// The status of another KubeVela Application.
	//
	// This is what lets one Application observe another - "is the platform's
	// ingress stack up before I render against it" - without the consumer knowing
	// that an Application is a CRD, or which fields of its status mean what.
	//
	// Status only. Nothing here reads `spec`: an Application's spec is its author's
	// intent and can carry properties they did not intend to publish, whereas its
	// status is observable state the controller already writes for anyone with read
	// access. Keeping to status makes this a health signal rather than a way to read
	// somebody else's configuration.
	//
	// `healthy` is the whole application, which is the question a consumer is
	// actually asking. An app with one unhealthy service is not healthy, and one
	// that has rendered no services yet is not healthy either - it is unknown, and
	// false is the safer of the two answers to act on.
	//
	// A component is reported once *per cluster it is placed in*: KubeVela keys a
	// service status on (name, namespace, cluster, env), so a topology policy
	// spreading one component across three clusters produces three entries with the
	// same name. That is why `components` is deduplicated and `services` is nested
	// by cluster - a flat list would repeat names, and a flat status would silently
	// describe whichever placement happened to sort first.
	//
	// A status entry with no cluster field is the hub, which reads as "local" here
	// rather than as the empty string it is stored as.
	schema: {
		name:      string
		namespace: string
		// The Application's phase - running, rendering, workflowSuspending...
		phase:   string
		healthy: bool
		// Absent until the first revision is created.
		revision?: string
		// Absent on an Application with no workflow status yet.
		workflow?: {
			mode:       string
			phase:      string
			suspend:    bool
			terminated: bool
			finished:   bool
			message:    string
		}
		// Component names, deduplicated across placements.
		components: [...string]
		// Distinct clusters the Application has components in. A single-cluster app
		// reports ["local"].
		clusters: [...string]
		// Per component, and within that per cluster it is placed in.
		services: [string]: {
			// Healthy in every cluster it is placed in, which is what "is this
			// component up" means once there is more than one of it.
			healthy: bool
			clusters: [string]: {
				healthy:   bool
				message:   string
				namespace: string
			}
		}
	}

	storage: {
		// Deliberately short, and deliberately not use-stale. Status is the one
		// thing where serving a cached answer is actively misleading: a consumer
		// gating on `healthy` wants to know it is unhealthy *now*, and stale health
		// is worse than a failed render because it is silently wrong.
		storageTTL:     "1m"
		onStaleFailure: "fail"
	}

	parameter: {
		// +usage=Name of the Application to read
		name: string
		// +usage=Namespace it lives in. Defaults to the consuming Application's own.
		namespace?: string
	}

	_ns: [
		if parameter.namespace != _|_ {parameter.namespace},
		context.namespace,
	][0]

	_app: kube.#Get & {
		$params: resource: {
			apiVersion: "core.oam.dev/v1beta1"
			kind:       "Application"
			metadata: {
				name:      parameter.name
				namespace: _ns
			}
		}
	}

	// An Application that exists but has not reconciled yet has no status. That is a
	// legitimate state to observe rather than an error - a consumer waiting for it
	// to come up needs to be able to see it not up. A missing Application, by
	// contrast, fails in kube.#Get, which is right.
	_status: *_app.$returns.status | {}
	_services: *_status.services | []

	// Normalise each placement: an absent or empty cluster field means the hub.
	_placed: [for s in _services {
		name: s.name
		cluster: [if (*s.cluster | "") != "" {*s.cluster | ""}, "local"][0]
		healthy:   s.healthy
		message:   *s.message | ""
		namespace: *s.namespace | ""
	}]

	// Deduplication by way of a struct: a component placed in three clusters is one
	// component, and a list comprehension would say three.
	_names: [for n in {for p in _placed {"\(p.name)": p.name}} {n}]
	_clusters: [for c in {for p in _placed {"\(p.cluster)": p.cluster}} {c}]

	output: {
		name:      parameter.name
		namespace: _ns
		phase:     *_status.status | "unknown"
		healthy: len(_services) > 0 && len([for s in _services if !s.healthy {s}]) == 0
		if _status.latestRevision != _|_ {
			revision: _status.latestRevision.name
		}
		if _status.workflow != _|_ {
			workflow: {
				mode:       *_status.workflow.mode | ""
				phase:      *_status.workflow.status | "unknown"
				suspend:    *_status.workflow.suspend | false
				terminated: *_status.workflow.terminated | false
				finished:   *_status.workflow.finished | false
				message:    *_status.workflow.message | ""
			}
		}
		components: _names
		clusters:   _clusters
		services: {
			for n in _names {
				"\(n)": {
					healthy: len([for p in _placed if p.name == n if !p.healthy {p}]) == 0
					clusters: {
						for p in _placed if p.name == n {
							"\(p.cluster)": {
								healthy:   p.healthy
								message:   p.message
								namespace: p.namespace
							}
						}
					}
				}
			}
		}
	}
}
