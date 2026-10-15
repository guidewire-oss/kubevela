import "vela/test"

// Loading the Application's components needs the Application, which a step test
// does not have, so they are mocked, already in dependsOn order as the real
// action returns them.
_app: mocks: {
	"vela/oam": {
		"#LoadComponetsInOrder": $returns: value: [
			{name: "s3-aux", type: "k8s-objects", properties: objects: [{kind: "CompositeResourceDefinition", metadata: name: "xbuckets"}]},
			{name: "s3-v1-aux", type: "k8s-objects", dependsOn: ["s3-aux"], properties: objects: [{kind: "Composition", metadata: annotations: "modules.oam.dev/placement": "hub"}]},
			{name: "s3-v1-aux-runtime", type: "k8s-objects", dependsOn: ["s3-aux"], properties: objects: [{kind: "Deployment", metadata: annotations: "modules.oam.dev/placement": "runtime"}]},
			{name: "s3-v1-defs", type: "k8s-objects", dependsOn: ["s3-v1-aux", "s3-v1-aux-runtime"], properties: objects: [{kind: "ComponentDefinition", metadata: name: "bucket"}]},
		]
		"#ApplyComponent": $returns: {}
	}
}

"applies hub components to the hub and runtime components to each cluster": test.#WorkflowStepExec & _app & {
	definition: "deploy-module"
	parameter: clusters: ["eu-1", "us-1"]
	expect: {
		phase: "succeeded"
		calls: "vela/oam": {
			"#ApplyComponent": [
				{$params: {cluster: "local", value: name: "s3-aux"}},
				{$params: {cluster: "local", value: name: "s3-v1-aux"}},
				{$params: {cluster: "eu-1", value: name: "s3-v1-aux-runtime"}},
				{$params: {cluster: "us-1", value: name: "s3-v1-aux-runtime"}},
				{$params: {cluster: "local", value: name: "s3-v1-defs"}},
			]
		}
	}
}

"definitions stay on the hub with a single runtime cluster": test.#WorkflowStepExec & _app & {
	definition: "deploy-module"
	parameter: clusters: ["eu-1"]
	expect: {
		phase: "succeeded"
		calls: "vela/oam": "#ApplyComponent": [
			{$params: {cluster: "local", value: name: "s3-aux"}},
			{$params: {cluster: "local", value: name: "s3-v1-aux"}},
			{$params: {cluster: "eu-1", value: name: "s3-v1-aux-runtime"}},
			{$params: {cluster: "local", value: name: "s3-v1-defs"}},
		]
	}
}
