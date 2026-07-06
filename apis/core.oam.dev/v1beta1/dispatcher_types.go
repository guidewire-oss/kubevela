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
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/condition"
)

// DispatcherSpec defines dispatcher template and metadata.
type DispatcherSpec struct {
	// Schematic defines the data format and template of dispatcher logic.
	// Only CUE schematic is supported for now.
	// +optional
	Schematic *common.Schematic `json:"schematic,omitempty"`
}

// DispatcherStatus is the status of Dispatcher.
type DispatcherStatus struct {
	// ConditionedStatus reflects the observed status of a resource.
	condition.ConditionedStatus `json:",inline"`
}

// SetConditions set condition for Dispatcher.
func (d *Dispatcher) SetConditions(c ...condition.Condition) {
	d.Status.SetConditions(c...)
}

// GetCondition gets condition from Dispatcher.
func (d *Dispatcher) GetCondition(conditionType condition.ConditionType) condition.Condition {
	return d.Status.GetCondition(conditionType)
}

// +kubebuilder:object:root=true

// Dispatcher is the Schema for the dispatchers API.
// +kubebuilder:resource:scope=Namespaced,categories={oam},shortName=dispatcher
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type Dispatcher struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DispatcherSpec   `json:"spec,omitempty"`
	Status DispatcherStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// DispatcherList contains a list of Dispatcher.
type DispatcherList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Dispatcher `json:"items"`
}

// DeepCopyInto copies this object into out.
func (in *Dispatcher) DeepCopyInto(out *Dispatcher) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	if in.Spec.Schematic != nil {
		out.Spec.Schematic = in.Spec.Schematic.DeepCopy()
	}
	in.Status.ConditionedStatus.DeepCopyInto(&out.Status.ConditionedStatus)
}

// DeepCopy creates a deep copy.
func (in *Dispatcher) DeepCopy() *Dispatcher {
	if in == nil {
		return nil
	}
	out := new(Dispatcher)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject creates a deep copy as runtime.Object.
func (in *Dispatcher) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies this list into out.
func (in *DispatcherList) DeepCopyInto(out *DispatcherList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]Dispatcher, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy creates a deep copy list.
func (in *DispatcherList) DeepCopy() *DispatcherList {
	if in == nil {
		return nil
	}
	out := new(DispatcherList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject creates a deep copy list as runtime.Object.
func (in *DispatcherList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
