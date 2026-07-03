/*
Copyright 2026.

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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EphemeralRunnerSetSpec defines the desired state of an EphemeralRunnerSet.
type EphemeralRunnerSetSpec struct {
	// Replicas is the desired number of ephemeral runners.
	Replicas int32 `json:"replicas"`

	// PatchID is a monotonically increasing token for listener/controller coordination.
	PatchID int64 `json:"patchID"`

	// MinRunners is the minimum number of runners (warm floor).
	MinRunners int32 `json:"minRunners,omitempty"`

	// MaxRunners is the maximum number of runners (hard ceiling).
	MaxRunners int32 `json:"maxRunners,omitempty"`
}

// EphemeralRunnerSetStatus defines the observed state of an EphemeralRunnerSet.
type EphemeralRunnerSetStatus struct {
	// TargetSize is the scaling decision output (desired runner count).
	TargetSize int32 `json:"targetSize,omitempty"`

	// TargetSizeUpdatedAt is when TargetSize was last updated.
	TargetSizeUpdatedAt *metav1.Time `json:"targetSizeUpdatedAt,omitempty"`

	// ReadyReplicas is the number of ready runners.
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// AvailableReplicas is the number of available runners.
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`

	// LastReconcilePatchID is the last patchID the controller reconciled.
	LastReconcilePatchID int64 `json:"lastReconcilePatchID,omitempty"`
}

// EphemeralRunnerSet is the Schema for the ephemeralrunnersets API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ersets
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.availableReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type EphemeralRunnerSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EphemeralRunnerSetSpec   `json:"spec,omitempty"`
	Status EphemeralRunnerSetStatus `json:"status,omitempty"`
}

// EphemeralRunnerSetList contains a list of EphemeralRunnerSet.
// +kubebuilder:object:root=true
type EphemeralRunnerSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EphemeralRunnerSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EphemeralRunnerSet{}, &EphemeralRunnerSetList{})
}
