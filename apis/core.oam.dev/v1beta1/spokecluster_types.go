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

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SpokeClusterMode is the lifecycle mode of a SpokeCluster.
type SpokeClusterMode string

const (
	// SpokeClusterModeConnect attaches and observes an existing cluster.
	SpokeClusterModeConnect SpokeClusterMode = "connect"
	// SpokeClusterModeProvision creates the cluster from a blueprint.
	SpokeClusterModeProvision SpokeClusterMode = "provision"
	// SpokeClusterModeAdopt imports an externally provisioned cluster.
	SpokeClusterModeAdopt SpokeClusterMode = "adopt"
)

// CredentialType keys the discriminated credential union.
type CredentialType string

const (
	// CredentialTypeKubeconfig connects via a kubeconfig held in a Secret.
	CredentialTypeKubeconfig CredentialType = "kubeconfig"
	// CredentialTypeAWS connects via AWS cloud-native identity.
	CredentialTypeAWS CredentialType = "aws"
	// CredentialTypeAzure connects via Azure cloud-native identity.
	CredentialTypeAzure CredentialType = "azure"
	// CredentialTypeGCP connects via GCP cloud-native identity.
	CredentialTypeGCP CredentialType = "gcp"
)

// AWSAuthMode is the AWS hub-to-spoke authentication mode.
type AWSAuthMode string

const (
	// AWSAuthModePodIdentity uses EKS Pod Identity.
	AWSAuthModePodIdentity AWSAuthMode = "podIdentity"
	// AWSAuthModeIRSA uses IAM Roles for Service Accounts.
	AWSAuthModeIRSA AWSAuthMode = "irsa"
)

// AzureAuthMode is the Azure hub-to-spoke authentication mode.
type AzureAuthMode string

const (
	// AzureAuthModeWorkloadIdentity uses Microsoft Entra Workload ID federation.
	AzureAuthModeWorkloadIdentity AzureAuthMode = "workloadIdentity"
	// AzureAuthModeManagedIdentity uses an assigned managed identity.
	AzureAuthModeManagedIdentity AzureAuthMode = "managedIdentity"
)

// GCPAuthMode is the GCP hub-to-spoke authentication mode.
type GCPAuthMode string

const (
	// GCPAuthModeWorkloadIdentityFederation uses Workload Identity Federation.
	GCPAuthModeWorkloadIdentityFederation GCPAuthMode = "workloadIdentityFederation"
	// GCPAuthModeServiceAccount impersonates a Google service account.
	GCPAuthModeServiceAccount GCPAuthMode = "serviceAccount"
)

// ConnectionState is the observed connectivity state of the spoke.
type ConnectionState string

const (
	// ConnectionStateConnected means the hub reached the spoke API server.
	ConnectionStateConnected ConnectionState = "Connected"
	// ConnectionStateDisconnected means the last probe failed.
	ConnectionStateDisconnected ConnectionState = "Disconnected"
	// ConnectionStateUnknown means the spoke has not yet been probed.
	ConnectionStateUnknown ConnectionState = "Unknown"
)

// SpokeDeletionPolicy controls what happens to a connected spoke's registration
// when its SpokeCluster is deleted.
type SpokeDeletionPolicy string

const (
	// SpokeDeletionPolicyDetach removes the hub-side registration on delete.
	SpokeDeletionPolicyDetach SpokeDeletionPolicy = "detach"
	// SpokeDeletionPolicyOrphan leaves the registration in place on delete.
	SpokeDeletionPolicyOrphan SpokeDeletionPolicy = "orphan"
)

// SpokeCluster status condition types. The reconcile loop sets all four; the constants live
// here so every consumer, controller, CLI and test alike, shares one spelling.
const (
	// SpokeClusterConditionRegistered is true once the hub-side registration exists.
	SpokeClusterConditionRegistered = "Registered"
	// SpokeClusterConditionCredentialValid is true once the credential materialized.
	SpokeClusterConditionCredentialValid = "CredentialValid"
	// SpokeClusterConditionConnected is true once the hub reached the spoke.
	SpokeClusterConditionConnected = "Connected"
	// SpokeClusterConditionInfoSynced is true once cluster inventory was discovered.
	SpokeClusterConditionInfoSynced = "InfoSynced"
)

// SpokeClusterSpec is the desired state of a managed cluster on the hub.
type SpokeClusterSpec struct {
	// Mode is the cluster lifecycle mode. Defaults to connect when unset.
	// +kubebuilder:validation:Enum=connect;provision;adopt
	// +kubebuilder:default=connect
	// +optional
	Mode SpokeClusterMode `json:"mode,omitempty"`

	// Credential is the hub-to-spoke connectivity credential, a discriminated
	// union keyed by type.
	Credential CredentialSpec `json:"credential"`

	// ProbeIntervalSeconds is how often the hub probes the spoke for reachability.
	// +optional
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=600
	ProbeIntervalSeconds int32 `json:"probeIntervalSeconds,omitempty"`

	// ProbeTimeoutSeconds is the per-probe timeout.
	// +optional
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=120
	ProbeTimeoutSeconds int32 `json:"probeTimeoutSeconds,omitempty"`

	// DeletionPolicy controls the fate of the hub-side registration on delete.
	// +optional
	// +kubebuilder:validation:Enum=detach;orphan
	// +kubebuilder:default=detach
	DeletionPolicy SpokeDeletionPolicy `json:"deletionPolicy,omitempty"`

	// InfraProvisioning references the shared cloud infrastructure the hub
	// reconciles against cloud APIs before the cluster is dispatched to (VPC,
	// IAM, DNS, and cluster creation when mode is provision).
	//
	// Phase 2 stub: defined so the schema is forward-compatible. No Phase 1
	// controller reconciles it. The admission webhook rejects it in connect mode.
	// +optional
	InfraProvisioning *InfraProvisioning `json:"infraProvisioning,omitempty"`

	// BlueprintRef references the ClusterBlueprint revision to dispatch to the
	// cluster.
	//
	// Phase 2 stub: accepted by admission and ignored. No Phase 1 controller
	// dispatches it.
	// +optional
	BlueprintRef *BlueprintReference `json:"blueprintRef,omitempty"`

	// RolloutStrategyRef references the ClusterRolloutStrategy that gates when a
	// new blueprint revision is dispatched to the cluster.
	//
	// Phase 2 stub: accepted by admission and ignored. No Phase 1 controller
	// reads it.
	// +optional
	RolloutStrategyRef *BlueprintReference `json:"rolloutStrategyRef,omitempty"`
}

// InfraProvisioning is the hub-reconciled shared cloud infrastructure for a
// SpokeCluster. It is applied on the hub against cloud APIs before any blueprint
// is dispatched to the spoke, and shared outputs are consumed by every
// SpokeCluster that references the same blueprint.
//
// Phase 2 stub: only the blueprint reference is modeled; provisioning behaviour
// and shared-output consumption arrive with the dispatch controller.
type InfraProvisioning struct {
	// BlueprintRef references the ClusterBlueprint that describes the shared
	// infrastructure to reconcile on the hub.
	// +optional
	BlueprintRef *BlueprintReference `json:"blueprintRef,omitempty"`
}

// BlueprintReference points at a KubeVela cluster-infrastructure object by name
// and an optional immutable revision. Blueprints are immutable once published,
// so revision pins an exact version to dispatch; leaving it empty tracks the
// object by name. It is the shared reference shape for the Phase 2 dispatch and
// rollout fields (blueprintRef, rolloutStrategyRef, and the blueprint references
// on Cluster), so a name-only consumer just omits revision.
type BlueprintReference struct {
	// Name of the referenced object.
	Name string `json:"name"`

	// Revision pins an immutable revision of the referenced object.
	// +optional
	Revision string `json:"revision,omitempty"`
}

// CredentialSpec is a discriminated union of hub-to-spoke credentials keyed by
// Type. Exactly one arm must be set.
type CredentialSpec struct {
	// Type selects the credential arm.
	// +kubebuilder:validation:Enum=kubeconfig;aws;azure;gcp
	Type CredentialType `json:"type"`

	// Kubeconfig holds a reference to a kubeconfig Secret.
	// +optional
	Kubeconfig *KubeconfigCredential `json:"kubeconfig,omitempty"`

	// AWS holds AWS cloud-native identity configuration.
	// +optional
	AWS *AWSCredential `json:"aws,omitempty"`

	// Azure holds Azure cloud-native identity configuration.
	// +optional
	Azure *AzureCredential `json:"azure,omitempty"`

	// GCP holds GCP cloud-native identity configuration.
	// +optional
	GCP *GCPCredential `json:"gcp,omitempty"`
}

// KubeconfigCredential connects to the spoke via a kubeconfig held in a Secret.
type KubeconfigCredential struct {
	// SecretRef points at the Secret holding the kubeconfig.
	SecretRef SecretKeyRef `json:"secretRef"`
}

// AWSCredential connects to an EKS cluster via AWS cloud-native identity.
type AWSCredential struct {
	// AuthMode is the AWS authentication mode.
	// +kubebuilder:validation:Enum=podIdentity;irsa
	AuthMode AWSAuthMode `json:"authMode"`

	// ClusterName is the EKS cluster name.
	ClusterName string `json:"clusterName"`

	// Region is the AWS region the cluster runs in.
	Region string `json:"region"`

	// RoleARN is the per-cluster IAM role the hub assumes.
	RoleARN string `json:"roleArn"`

	// ExternalID is an optional STS external id for the role assumption.
	// +optional
	ExternalID string `json:"externalId,omitempty"`
}

// AzureCredential connects to an AKS cluster via Azure cloud-native identity.
//
// Phase 1 placeholder: the CRD enum keeps this arm so the schema is
// forward-compatible. The Phase 1 admission webhook rejects type azure.
// No provider materializes it. The fields mirror what an AKS provider
// needs to locate the cluster and name the workload identity the hub
// federates to. See the aws arm for the working reference.
type AzureCredential struct {
	// AuthMode is the Azure authentication mode.
	// +kubebuilder:validation:Enum=workloadIdentity;managedIdentity
	AuthMode AzureAuthMode `json:"authMode"`

	// SubscriptionID is the Azure subscription that owns the AKS cluster.
	SubscriptionID string `json:"subscriptionID"`

	// ResourceGroup is the resource group the AKS cluster lives in.
	ResourceGroup string `json:"resourceGroup"`

	// ClusterName is the AKS cluster name.
	ClusterName string `json:"clusterName"`

	// TenantID is the Entra tenant the federated identity belongs to.
	// +optional
	TenantID string `json:"tenantID,omitempty"`

	// ClientID is the workload or managed identity the hub authenticates as.
	// +optional
	ClientID string `json:"clientID,omitempty"`
}

// GCPCredential connects to a GKE cluster via GCP cloud-native identity.
//
// Phase 1 placeholder: the CRD enum keeps this arm so the schema is
// forward-compatible. The Phase 1 admission webhook rejects type gcp.
// No provider materializes it. The fields mirror what a GKE provider
// needs to locate the cluster and name the service account the hub
// impersonates. See the aws arm for the working reference.
type GCPCredential struct {
	// AuthMode is the GCP authentication mode.
	// +kubebuilder:validation:Enum=workloadIdentityFederation;serviceAccount
	AuthMode GCPAuthMode `json:"authMode"`

	// ProjectID is the GCP project that owns the GKE cluster.
	ProjectID string `json:"projectID"`

	// Location is the cluster's region or zone.
	Location string `json:"location"`

	// ClusterName is the GKE cluster name.
	ClusterName string `json:"clusterName"`

	// ServiceAccountEmail is the Google service account the hub impersonates.
	// +optional
	ServiceAccountEmail string `json:"serviceAccountEmail,omitempty"`
}

// SecretKeyRef references a key within a Secret.
type SecretKeyRef struct {
	// Name is the Secret name.
	Name string `json:"name"`

	// Namespace is the Secret namespace. If set, it must match the
	// SpokeCluster namespace. The admission webhook rejects any other
	// value. Omit it to use the SpokeCluster's namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Key is the data key within the Secret. Defaults to "kubeconfig".
	// +optional
	Key string `json:"key,omitempty"`
}

// SpokeClusterStatus is the observed state of a managed cluster.
type SpokeClusterStatus struct {
	// ObservedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Connection is the observed connectivity state.
	// +kubebuilder:validation:Enum=Connected;Disconnected;Unknown
	// +optional
	Connection ConnectionState `json:"connection,omitempty"`

	// Conditions is the list of standard Kubernetes conditions.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ClusterInfo is the discovered inventory of the spoke.
	// +optional
	ClusterInfo *SpokeClusterInfo `json:"clusterInfo,omitempty"`

	// LastProbeTime is when the hub last wrote a probe observation into status.
	// Healthy passes refresh it at most every five minutes so the LAST PROBE
	// column stays recent without an etcd write on every probe interval.
	// Connection flips and probe failures update it immediately.
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`

	// DispatchedRevision is the blueprint revision the hub last dispatched to the
	// spoke. The dispatch controller advances a spoke when this differs from
	// spec.blueprintRef.revision.
	//
	// Phase 2 stub: written by the dispatch controller once blueprint dispatch is
	// built; empty in connect-only Phase 1.
	// +optional
	DispatchedRevision string `json:"dispatchedRevision,omitempty"`

	// Health is the spoke's blueprint health, pulled from the spoke Cluster on
	// demand while connected. The spoke never pushes it to the hub.
	//
	// Phase 2 stub: populated once blueprint dispatch and spoke-side health
	// aggregation exist; nil in connect-only Phase 1.
	// +optional
	Health *SpokeClusterHealth `json:"health,omitempty"`
}

// SpokeClusterHealth is the blueprint health the hub pulls from the spoke
// Cluster on demand. It summarizes how many of the dispatched blueprint's planes
// are healthy at the time of the last pull.
//
// Phase 2 stub: no controller pulls this in Phase 1.
type SpokeClusterHealth struct {
	// Status is the aggregate blueprint health of the spoke (for example Healthy
	// or Degraded).
	// +optional
	Status string `json:"status,omitempty"`

	// PlanesHealthy is the number of blueprint planes reporting healthy.
	// +optional
	PlanesHealthy int `json:"planesHealthy,omitempty"`

	// PlanesTotal is the total number of blueprint planes on the spoke.
	// +optional
	PlanesTotal int `json:"planesTotal,omitempty"`

	// LastPulledAt is when the hub last pulled health from the spoke.
	// +optional
	LastPulledAt *metav1.Time `json:"lastPulledAt,omitempty"`
}

// SpokeClusterInfo is the discovered inventory of a managed cluster.
type SpokeClusterInfo struct {
	// KubernetesVersion is the spoke API server version.
	// +optional
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`

	// Platform is the discovered cluster flavour (eks, gke, aks, kind, k3s).
	// +optional
	Platform string `json:"platform,omitempty"`

	// Region is the cloud region the spoke runs in.
	// +optional
	Region string `json:"region,omitempty"`

	// NodeCount is the number of nodes in the spoke.
	// +optional
	NodeCount int `json:"nodeCount,omitempty"`

	// TotalCPU is the aggregate allocatable CPU across nodes.
	// +optional
	TotalCPU string `json:"totalCPU,omitempty"`

	// TotalMemory is the aggregate allocatable memory across nodes.
	// +optional
	TotalMemory string `json:"totalMemory,omitempty"`

	// APIServerEndpoint is the spoke API server endpoint.
	// +optional
	APIServerEndpoint string `json:"apiServerEndpoint,omitempty"`

	// LatencyMillis is the last observed hub-to-spoke round-trip latency.
	// +optional
	LatencyMillis int64 `json:"latencyMillis,omitempty"`

	// LastSyncedTime is when this inventory was last discovered successfully.
	//
	// It advances only on a successful discovery, so it does not move while the spoke is
	// unreachable (discovery is skipped) or while discovery is failing. That is what makes
	// it the field to read for staleness: the InfoSynced condition's lastTransitionTime
	// records when discovery first started succeeding, not when it last did.
	// status.lastProbeTime is refreshed on a coarser cadence (see probeHeartbeatInterval).
	// +optional
	LastSyncedTime *metav1.Time `json:"lastSyncedTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories={oam},shortName=spc
// +kubebuilder:printcolumn:name="MODE",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="VERSION",type=string,JSONPath=`.status.clusterInfo.kubernetesVersion`
// +kubebuilder:printcolumn:name="NODES",type=integer,JSONPath=`.status.clusterInfo.nodeCount`
// +kubebuilder:printcolumn:name="PLATFORM",type=string,JSONPath=`.status.clusterInfo.platform`
// +kubebuilder:printcolumn:name="STATUS",type=string,JSONPath=`.status.connection`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="REGION",type=string,JSONPath=`.status.clusterInfo.region`,priority=1
// +kubebuilder:printcolumn:name="ENDPOINT",type=string,JSONPath=`.status.clusterInfo.apiServerEndpoint`,priority=1
// +kubebuilder:printcolumn:name="CPU",type=string,JSONPath=`.status.clusterInfo.totalCPU`,priority=1
// +kubebuilder:printcolumn:name="MEMORY",type=string,JSONPath=`.status.clusterInfo.totalMemory`,priority=1
// +kubebuilder:printcolumn:name="LATENCY",type=integer,JSONPath=`.status.clusterInfo.latencyMillis`,priority=1
// +kubebuilder:printcolumn:name="SYNCED",type=date,JSONPath=`.status.clusterInfo.lastSyncedTime`,priority=1
// +kubebuilder:printcolumn:name="AUTH",type=string,JSONPath=`.spec.credential.type`,priority=1
// +kubebuilder:printcolumn:name="LAST PROBE",type=date,JSONPath=`.status.lastProbeTime`,priority=1
// +kubebuilder:validation:XValidation:rule="self.metadata.name != 'local'",message="name must not be the reserved local cluster name"
// +kubebuilder:validation:XValidation:rule="self.spec.mode == 'connect'",message="mode must be 'connect' in Phase 1 (provision and adopt are not supported yet)"
// +kubebuilder:validation:XValidation:rule="self.spec.credential.type in ['kubeconfig', 'aws']",message="credential.type must be kubeconfig or aws in Phase 1"
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SpokeCluster is the first-class hub representation of one managed cluster. A
// cluster acting as a hub for downstream clusters can hold SpokeCluster
// objects at any level of the tree.
type SpokeCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SpokeClusterSpec   `json:"spec,omitempty"`
	Status SpokeClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SpokeClusterList contains a list of SpokeCluster.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type SpokeClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SpokeCluster `json:"items"`
}
