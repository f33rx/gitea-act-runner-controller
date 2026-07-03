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

package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
)

// ADR 0008 Decision 6: a GiteaRunnerSet override wins over the manager-wide default;
// with neither set, the resolved value is nil (the check is disabled for that runner).

func TestResolveActiveDeadlineSeconds(t *testing.T) {
	override := int64(3600)

	cases := []struct {
		name           string
		grsOverride    *int64
		managerDefault int64
		wantNil        bool
		want           int64
	}{
		{"override wins over default", &override, 7200, false, 3600},
		{"falls back to default when unset", nil, 7200, false, 7200},
		{"nil when neither is set", nil, 0, true, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &EphemeralRunnerSetReconciler{DefaultActiveDeadlineSeconds: tc.managerDefault}
			grs := &giteaactionsv1alpha1.GiteaRunnerSet{
				Spec: giteaactionsv1alpha1.GiteaRunnerSetSpec{ActiveDeadlineSeconds: tc.grsOverride},
			}

			got := r.resolveActiveDeadlineSeconds(grs)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %d", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected %d, got nil", tc.want)
			}
			if *got != tc.want {
				t.Fatalf("expected %d, got %d", tc.want, *got)
			}
		})
	}
}

func TestResolveStallWindow(t *testing.T) {
	override := metav1.Duration{Duration: 10 * time.Minute}

	cases := []struct {
		name           string
		grsOverride    *metav1.Duration
		managerDefault time.Duration
		wantNil        bool
		want           time.Duration
	}{
		{"override wins over default", &override, 20 * time.Minute, false, 10 * time.Minute},
		{"falls back to default when unset", nil, 20 * time.Minute, false, 20 * time.Minute},
		{"nil when neither is set", nil, 0, true, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &EphemeralRunnerSetReconciler{DefaultStallWindow: tc.managerDefault}
			grs := &giteaactionsv1alpha1.GiteaRunnerSet{
				Spec: giteaactionsv1alpha1.GiteaRunnerSetSpec{StallWindow: tc.grsOverride},
			}

			got := r.resolveStallWindow(grs)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %s", got.Duration)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected %s, got nil", tc.want)
			}
			if got.Duration != tc.want {
				t.Fatalf("expected %s, got %s", tc.want, got.Duration)
			}
		})
	}
}

func TestResolvePendingTimeout(t *testing.T) {
	override := metav1.Duration{Duration: 2 * time.Minute}

	cases := []struct {
		name           string
		grsOverride    *metav1.Duration
		managerDefault time.Duration
		wantNil        bool
		want           time.Duration
	}{
		{"override wins over default", &override, 5 * time.Minute, false, 2 * time.Minute},
		{"falls back to default when unset", nil, 5 * time.Minute, false, 5 * time.Minute},
		{"nil when neither is set", nil, 0, true, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &EphemeralRunnerSetReconciler{DefaultPendingTimeout: tc.managerDefault}
			grs := &giteaactionsv1alpha1.GiteaRunnerSet{
				Spec: giteaactionsv1alpha1.GiteaRunnerSetSpec{PendingTimeout: tc.grsOverride},
			}

			got := r.resolvePendingTimeout(grs)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %s", got.Duration)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected %s, got nil", tc.want)
			}
			if got.Duration != tc.want {
				t.Fatalf("expected %s, got %s", tc.want, got.Duration)
			}
		})
	}
}
