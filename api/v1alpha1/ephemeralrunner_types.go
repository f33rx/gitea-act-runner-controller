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

// EphemeralRunnerPhase is the phase of an ephemeral runner.
type EphemeralRunnerPhase string

const (
	EphemeralRunnerPending   EphemeralRunnerPhase = "Pending"
	EphemeralRunnerRunning   EphemeralRunnerPhase = "Running"
	EphemeralRunnerSucceeded EphemeralRunnerPhase = "Succeeded"
	EphemeralRunnerFailed    EphemeralRunnerPhase = "Failed"
)

// EphemeralRunnerSpec defines the desired state of an EphemeralRunner.
type EphemeralRunnerSpec struct {
	// GiteaConfigURL is the URL of the Gitea instance.
	GiteaConfigURL string `json:"giteaConfigUrl"`

	// RegistrationToken is the per-pod registration token (from Secret).
	RegistrationToken string `json:"registrationToken"`

	// Labels are the runner labels.
	Labels []string `json:"labels"`

	// RunnerScope is the scope of the runner: org or instance.
	RunnerScope string `json:"runnerScope"`

	// OrgName is the Gitea organization name (for org-scoped runners).
	OrgName string `json:"orgName"`

	// GiteaRunnerSetName is the name of the parent GiteaRunnerSet.
	GiteaRunnerSetName string `json:"giteaRunnerSetName"`
}

// EphemeralRunnerStatus defines the observed state of an EphemeralRunner.
type EphemeralRunnerStatus struct {
	// Phase is the current phase of the runner.
	Phase EphemeralRunnerPhase `json:"phase,omitempty"`

	// Reason is a message explaining the current phase.
	Reason string `json:"reason,omitempty"`

	// RunnerID is the Gitea runner ID assigned at registration.
	RunnerID int64 `json:"runnerId,omitempty"`

	// RunnerUUID is the Gitea runner UUID assigned at registration.
	RunnerUUID string `json:"runnerUUID,omitempty"`

	// JobRef is the current task/job the runner claimed.
	JobRef string `json:"jobRef,omitempty"`

	// PodName is the name of the associated Pod.
	PodName string `json:"podName,omitempty"`

	// LastObservedTime is the last time the runner was observed.
	LastObservedTime *metav1.Time `json:"lastObservedTime,omitempty"`
}

// EphemeralRunner is the Schema for the ephemeralrunners API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=er;ers
type EphemeralRunner struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EphemeralRunnerSpec   `json:"spec,omitempty"`
	Status EphemeralRunnerStatus `json:"status,omitempty"`
}

// EphemeralRunnerList contains a list of EphemeralRunner.
// +kubebuilder:object:root=true
type EphemeralRunnerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EphemeralRunner `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EphemeralRunner{}, &EphemeralRunnerList{})
}
