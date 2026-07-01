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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GiteaRunnerSetSpec defines the desired state of GiteaRunnerSet.
type GiteaRunnerSetSpec struct {
	// GiteaConfigUrl is the URL of the Gitea instance.
	GiteaConfigUrl string `json:"giteaConfigUrl"`

	// GiteaConfigSecretRef is a reference to the Secret containing the Gitea operator credential.
	GiteaConfigSecretRef corev1.SecretKeySelector `json:"giteaConfigSecretRef"`

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

	// Template is the pod template spec for the runner pods.
	Template corev1.PodTemplateSpec `json:"template"`
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
