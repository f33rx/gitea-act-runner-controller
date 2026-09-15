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

	// ActiveDeadlineSeconds is the resolved hard cap on total pod lifetime (ADR 0008),
	// copied down from the GiteaRunnerSet override or the manager default at creation
	// time so each EphemeralRunner is self-describing. Applied to the Pod spec verbatim;
	// the kubelet enforces it.
	// +optional
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`

	// StallWindow is the resolved stall-detection window (ADR 0008), copied down from
	// the GiteaRunnerSet override or the manager default at creation time.
	// +optional
	StallWindow *metav1.Duration `json:"stallWindow,omitempty"`

	// PendingTimeout is the resolved pre-claim (Pending) timeout (ADR 0008), copied down
	// from the GiteaRunnerSet override or the manager default at creation time.
	// +optional
	PendingTimeout *metav1.Duration `json:"pendingTimeout,omitempty"`
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

	// LastObservedTime is the last time the runner's phase or reason changed.
	LastObservedTime *metav1.Time `json:"lastObservedTime,omitempty"`

	// PhaseStartTime is when the runner entered its current Phase. ADR 0008 uses this
	// to measure how long a runner has been Pending (pre-claim timeout), independent of
	// unrelated Reason-only status updates that also bump LastObservedTime.
	PhaseStartTime *metav1.Time `json:"phaseStartTime,omitempty"`

	// LastProgressTime is the last time the runner's claimed Gitea job log grew (ADR
	// 0008's stall-detection liveness signal for a Running runner: any new step output
	// means the job is doing something, whether or not the pod phase itself changed).
	// act_runner streams step output to Gitea via its own protocol independent of the
	// runner container's stdout, so this is read from Gitea's job-log endpoint, not
	// kubectl logs. A Running runner with no LastProgressTime movement for the
	// configured stall window is presumed stuck.
	LastProgressTime *metav1.Time `json:"lastProgressTime,omitempty"`

	// LastJobLogSize is the Gitea job-log Content-Length observed at LastProgressTime,
	// used to detect growth on the next check via a cheap header-only comparison instead
	// of re-downloading and diffing the full (growing) log body.
	LastJobLogSize int64 `json:"lastJobLogSize,omitempty"`
}

// EphemeralRunner is the Schema for the ephemeralrunners API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=er;ers
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=`.status.podName`
// +kubebuilder:printcolumn:name="Job",type=string,JSONPath=`.status.jobRef`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.reason`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
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
