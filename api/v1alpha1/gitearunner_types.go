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
	"k8s.io/apimachinery/pkg/runtime"
)

// SecretKeySelector selects a key of a Secret. It mirrors the fields of
// corev1.SecretKeySelector that this operator uses, defined locally so
// controller-gen produces a fully structural CRD schema. Embedding the core
// type instead forces a $ref, which the API server rejects in a CRD schema.
type SecretKeySelector struct {
	// Name of the referent Secret (same namespace as the GiteaRunnerSet).
	Name string `json:"name"`

	// Key within the Secret whose value is the credential.
	Key string `json:"key"`

	// Optional marks whether the Secret or its key must exist.
	// +optional
	Optional *bool `json:"optional,omitempty"`
}

// GiteaRunnerSetSpec defines the desired state of GiteaRunnerSet.
type GiteaRunnerSetSpec struct {
	// GiteaConfigURL is the URL of the Gitea instance.
	GiteaConfigURL string `json:"giteaConfigUrl"`

	// GiteaConfigSecretRef is a reference to the Secret containing the Gitea operator credential.
	GiteaConfigSecretRef SecretKeySelector `json:"giteaConfigSecretRef"`

	// RunnerScope is the scope of the runner: org or instance.
	// +kubebuilder:validation:Enum=org;instance
	RunnerScope string `json:"runnerScope"`

	// OrgName is the Gitea organization name (for org-scoped runners).
	OrgName string `json:"orgName"`

	// Labels are the runner labels (e.g., ubuntu-latest, dind).
	Labels []string `json:"labels"`

	// MinRunners is the minimum number of idle ephemeral runners to keep.
	MinRunners int32 `json:"minRunners,omitempty"`

	// MaxRunners is the maximum number of ephemeral runners to create.
	MaxRunners int32 `json:"maxRunners,omitempty"`

	// Template is the pod template spec for the runner pods (a corev1.PodTemplateSpec).
	// Held as a RawExtension with preserved unknown fields so controller-gen emits a
	// structural schema (x-kubernetes-preserve-unknown-fields) rather than a $ref to the
	// embedded core type. Not consumed yet; optional.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Template *runtime.RawExtension `json:"template,omitempty"`

	// ActiveDeadlineSeconds overrides the manager-wide default hard cap on total runner
	// pod lifetime (ADR 0008). Set as the Pod's activeDeadlineSeconds; the kubelet
	// enforces it independent of any operator logic. Omit to use the manager default.
	// +optional
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`

	// StallWindow overrides the manager-wide default stall-detection window (ADR 0008):
	// how long a Running runner may show no progress signal (pod-phase/condition
	// transitions) before the operator treats the job as stuck, fails it, and tears the
	// runner down. Omit to use the manager default.
	// +optional
	StallWindow *metav1.Duration `json:"stallWindow,omitempty"`

	// PendingTimeout overrides the manager-wide default for how long a runner may stay
	// Pending (never claimed a job) before the operator treats pod creation as failed
	// and retries with capped backoff (ADR 0008). Omit to use the manager default.
	// +optional
	PendingTimeout *metav1.Duration `json:"pendingTimeout,omitempty"`
}

// GiteaRunnerSetStatus defines the observed state of GiteaRunnerSet.
type GiteaRunnerSetStatus struct {
	// ReadyReplicas is the number of ready runner pods.
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// AvailableReplicas is the number of available runner pods.
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`
}

// GiteaRunnerSet is the Schema for the gitearunnersets API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=grs;grsets
// +kubebuilder:printcolumn:name="Min",type=integer,JSONPath=`.spec.minRunners`
// +kubebuilder:printcolumn:name="Max",type=integer,JSONPath=`.spec.maxRunners`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.availableReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type GiteaRunnerSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GiteaRunnerSetSpec   `json:"spec,omitempty"`
	Status GiteaRunnerSetStatus `json:"status,omitempty"`
}

// GiteaRunnerSetList contains a list of GiteaRunnerSet.
// +kubebuilder:object:root=true
type GiteaRunnerSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GiteaRunnerSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GiteaRunnerSet{}, &GiteaRunnerSetList{})
}
