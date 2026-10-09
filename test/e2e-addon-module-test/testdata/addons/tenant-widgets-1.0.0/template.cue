// A hand-written type: module component. It sets no namespace: a module's
// definitions install into the namespace of the Application that installs the
// addon, which the scenario applies into kit-tenant.
package main

output: {
	apiVersion: "core.oam.dev/v1beta1"
	kind:       "Application"
	spec: components: [{
		name: "tenant-kit"
		type: "module"
		properties: {
			module:   "widget-kit"
			registry: "e2e-modules"
			version:  "1.0.0"
		}
	}]
}
