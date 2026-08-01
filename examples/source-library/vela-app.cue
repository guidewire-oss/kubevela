import "vela/kube"

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
	// The component names the Application reports, in status order.
	components: [...string]
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
_status:   *_app.$returns.status | {}
_services: *_status.services | []

output: {
	name:      parameter.name
	namespace: _ns
	phase:     *_status.status | "unknown"
	healthy:   len(_services) > 0 && len([for s in _services if !s.healthy {s}]) == 0
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
	components: [for s in _services {s.name}]
}
