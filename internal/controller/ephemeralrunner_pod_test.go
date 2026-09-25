package controller

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
)

func podFixture(t *testing.T, r *EphemeralRunnerReconciler, template string) *corev1.Pod {
	t.Helper()
	runner := &giteaactionsv1alpha1.EphemeralRunner{
		ObjectMeta: metav1.ObjectMeta{Name: "set-runner-0", Namespace: "gitea-runners"},
		Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{
			GiteaConfigURL:     "http://gitea:3000",
			Labels:             []string{"ubuntu-latest"},
			GiteaRunnerSetName: "set",
		},
	}
	if template != "" {
		runner.Spec.Template = &runtime.RawExtension{Raw: []byte(template)}
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tok", Namespace: "gitea-runners"}}
	pod, err := r.constructPod(context.Background(), runner, secret)
	if err != nil {
		t.Fatalf("constructPod: %v", err)
	}
	return pod
}

func envValue(c corev1.Container, name string) (string, int) {
	val, n := "", 0
	for _, e := range c.Env {
		if e.Name == name {
			val = e.Value
			n++
		}
	}
	return val, n
}

// recordLogProgress matches in-progress jobs by the name act_runner registered under.
// Left to its default that name is the pod hostname, which the kubelet truncates at 63
// characters, so a long runner set name would silently break job matching. Pin it.
func TestConstructPod_PinsRunnerName(t *testing.T) {
	scheme := newTestScheme(t)
	r := &EphemeralRunnerReconciler{Scheme: scheme}
	runner := &giteaactionsv1alpha1.EphemeralRunner{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "a-very-long-gitearunnerset-name-that-exceeds-the-hostname-limit-runner-0",
			Namespace: "gitea-runners",
		},
		Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{Labels: []string{"ubuntu-latest"}},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tok", Namespace: "gitea-runners"}}

	pod, err := r.constructPod(context.Background(), runner, secret)
	if err != nil {
		t.Fatalf("constructPod: %v", err)
	}

	var got string
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == envGiteaRunnerName {
			got = e.Value
		}
	}
	if got != runner.Name {
		t.Fatalf("%s = %q, want %q", envGiteaRunnerName, got, runner.Name)
	}
	if len(runner.Name) <= 63 {
		t.Fatal("fixture name must exceed the 63-char hostname limit to be meaningful")
	}
}

// Runner sets created before templates were honored must get the same pod as before.
func TestConstructPod_NoTemplateKeepsDefaults(t *testing.T) {
	pod := podFixture(t, &EphemeralRunnerReconciler{}, "")

	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(pod.Spec.Containers))
	}
	c := pod.Spec.Containers[0]
	if c.Name != runnerContainerName || c.Image != DefaultRunnerImage {
		t.Fatalf("container = %s/%s, want %s/%s", c.Name, c.Image, runnerContainerName, DefaultRunnerImage)
	}
	if pod.Spec.ServiceAccountName != DefaultRunnerServiceAccountName {
		t.Fatalf("serviceAccountName = %q", pod.Spec.ServiceAccountName)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("restartPolicy = %q", pod.Spec.RestartPolicy)
	}
	if v, _ := envValue(c, "GITEA_RUNNER_LABELS"); v != "ubuntu-latest:host" {
		t.Fatalf("GITEA_RUNNER_LABELS = %q", v)
	}
	if len(c.Env) != 6 {
		t.Fatalf("env count = %d, want 6", len(c.Env))
	}
}

func TestConstructPod_DefaultImageFlag(t *testing.T) {
	pod := podFixture(t, &EphemeralRunnerReconciler{RunnerImage: "example/runner:1"}, "")
	if got := pod.Spec.Containers[0].Image; got != "example/runner:1" {
		t.Fatalf("image = %q, want the --default-runner-image value", got)
	}
}

func TestConstructPod_TemplatePassesThrough(t *testing.T) {
	pod := podFixture(t, &EphemeralRunnerReconciler{RunnerImage: "example/runner:1"}, `{
	  "metadata": {"annotations": {"a": "b"}},
	  "spec": {
	    "serviceAccountName": "custom-sa",
	    "nodeSelector": {"cloud.google.com/compute-class": "runners"},
	    "securityContext": {"runAsNonRoot": true},
	    "volumes": [{"name": "work", "emptyDir": {}}],
	    "containers": [{
	      "name": "act-runner",
	      "image": "example/runner-node:2",
	      "resources": {"requests": {"cpu": "2", "memory": "4Gi", "ephemeral-storage": "20Gi"}},
	      "volumeMounts": [{"name": "work", "mountPath": "/work"}]
	    }]
	  }
	}`)

	c := pod.Spec.Containers[0]
	if c.Image != "example/runner-node:2" {
		t.Fatalf("image = %q, template image must win over the default", c.Image)
	}
	if got := c.Resources.Requests[corev1.ResourceEphemeralStorage]; got.Cmp(resource.MustParse("20Gi")) != 0 {
		t.Fatalf("ephemeral-storage request = %s", got.String())
	}
	if len(c.VolumeMounts) != 1 || len(pod.Spec.Volumes) != 1 {
		t.Fatal("volumes and mounts must pass through")
	}
	if pod.Spec.ServiceAccountName != "custom-sa" {
		t.Fatalf("serviceAccountName = %q, template value must win", pod.Spec.ServiceAccountName)
	}
	if pod.Spec.NodeSelector["cloud.google.com/compute-class"] != "runners" {
		t.Fatal("nodeSelector must pass through")
	}
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsNonRoot == nil {
		t.Fatal("pod securityContext must pass through")
	}
	if pod.Annotations["a"] != "b" {
		t.Fatal("template annotations must pass through")
	}
}

// garc finds pods by name and labels, keeps them ephemeral with restartPolicy Never, and
// relies on its env vars for registration; a template must not be able to change those.
func TestConstructPod_ForcesGarcOwnedFields(t *testing.T) {
	pod := podFixture(t, &EphemeralRunnerReconciler{}, `{
	  "metadata": {"name": "other", "labels": {"app": "mine", "team": "x"}},
	  "spec": {
	    "restartPolicy": "Always",
	    "activeDeadlineSeconds": 5,
	    "containers": [{
	      "name": "act-runner",
	      "env": [
	        {"name": "GITEA_INSTANCE_URL", "value": "http://wrong"},
	        {"name": "GITEA_RUNNER_NAME", "value": "wrong"},
	        {"name": "EXTRA", "value": "kept"}
	      ]
	    }]
	  }
	}`)

	if pod.Name != "set-runner-0" || pod.Namespace != "gitea-runners" {
		t.Fatalf("pod = %s/%s", pod.Namespace, pod.Name)
	}
	if pod.Labels["app"] != "gitea-runner" || pod.Labels["ephemeral-runner"] != "set-runner-0" || pod.Labels["gitearunner-set"] != "set" {
		t.Fatalf("garc labels not forced: %v", pod.Labels)
	}
	if pod.Labels["team"] != "x" {
		t.Fatal("other template labels must be kept")
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("restartPolicy = %q, want Never", pod.Spec.RestartPolicy)
	}
	if pod.Spec.ActiveDeadlineSeconds != nil {
		t.Fatal("activeDeadlineSeconds must come from the resolved runner spec, not the template")
	}
	c := pod.Spec.Containers[0]
	if v, n := envValue(c, "GITEA_INSTANCE_URL"); v != "http://gitea:3000" || n != 1 {
		t.Fatalf("GITEA_INSTANCE_URL = %q (x%d)", v, n)
	}
	if v, n := envValue(c, envGiteaRunnerName); v != "set-runner-0" || n != 1 {
		t.Fatalf("%s = %q (x%d)", envGiteaRunnerName, v, n)
	}
	if v, _ := envValue(c, "EXTRA"); v != "kept" {
		t.Fatal("non-garc env vars must be kept")
	}
}

func TestConstructPod_RunnerContainerSelection(t *testing.T) {
	// The single container is the runner whatever its name; a native sidecar in
	// initContainers is left alone.
	pod := podFixture(t, &EphemeralRunnerReconciler{}, `{"spec": {
	  "initContainers": [{"name": "sidecar", "image": "busybox", "restartPolicy": "Always"}],
	  "containers": [{"name": "runner", "image": "example/r:1"}]
	}}`)
	c := pod.Spec.Containers[0]
	if _, n := envValue(c, envGiteaRunnerName); n != 1 || c.Image != "example/r:1" {
		t.Fatal("the template's container must be treated as the runner")
	}
	sc := pod.Spec.InitContainers[0]
	if len(sc.Env) != 0 || sc.Image != "busybox" || sc.RestartPolicy == nil {
		t.Fatalf("native sidecar must be untouched: %+v", sc)
	}

	// A template with no containers still gets one.
	pod = podFixture(t, &EphemeralRunnerReconciler{}, `{"spec": {"nodeSelector": {"k": "v"}}}`)
	if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != runnerContainerName {
		t.Fatalf("containers = %+v", pod.Spec.Containers)
	}
}

func TestConstructPod_TemplateCannotOverrideGarcLabelsOrDeadline(t *testing.T) {
	deadline := int64(3600)
	runner := &giteaactionsv1alpha1.EphemeralRunner{
		ObjectMeta: metav1.ObjectMeta{Name: "set-runner-0", Namespace: "gitea-runners"},
		Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{
			GiteaRunnerSetName:    "set",
			ActiveDeadlineSeconds: &deadline,
			Template: &runtime.RawExtension{Raw: []byte(`{
			  "metadata": {"labels": {"ephemeral-runner": "x", "gitearunner-set": "y"}},
			  "spec": {"activeDeadlineSeconds": 5}
			}`)},
		},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tok"}}
	pod, err := (&EphemeralRunnerReconciler{}).constructPod(context.Background(), runner, secret)
	if err != nil {
		t.Fatal(err)
	}
	if pod.Labels["ephemeral-runner"] != "set-runner-0" || pod.Labels["gitearunner-set"] != "set" {
		t.Fatalf("labels = %v", pod.Labels)
	}
	if pod.Spec.ActiveDeadlineSeconds == nil || *pod.Spec.ActiveDeadlineSeconds != deadline {
		t.Fatalf("activeDeadlineSeconds = %v, want the resolved %d", pod.Spec.ActiveDeadlineSeconds, deadline)
	}
}

func TestDecodeRunnerTemplate(t *testing.T) {
	cases := []struct {
		name, raw string
		wantErr   bool
	}{
		{"misspelled field", `{"spec": {"containerz": []}}`, true},
		{"wrong-case field", `{"spec": {"Containers": []}}`, true},
		{"duplicate field", `{"spec": {"nodeSelector": {}, "nodeSelector": {}}}`, true},
		// A regular sidecar keeps the pod Running after act_runner exits, so teardown
		// never fires.
		{"second container", `{"spec": {"containers": [{"name": "a"}, {"name": "b"}]}}`, true},
		{"null creationTimestamp as stored by the API server", `{"metadata": {"creationTimestamp": null}, "spec": {}}`, false},
		{"native sidecar", `{"spec": {"initContainers": [{"name": "s", "restartPolicy": "Always"}], "containers": [{"name": "a"}]}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeRunnerTemplate(&runtime.RawExtension{Raw: []byte(tc.raw)})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
	if _, err := decodeRunnerTemplate(nil); err != nil {
		t.Fatalf("nil template: %v", err)
	}
}

func TestConstructPod_DefaultResources(t *testing.T) {
	// The shape the chart renders into --default-runner-resources; --set turns cpu into a
	// JSON number.
	var def corev1.ResourceRequirements
	if err := json.Unmarshal([]byte(`{"requests":{"cpu":1,"memory":"2Gi","ephemeral-storage":"4Gi"},"limits":{"memory":"2Gi","ephemeral-storage":"4Gi"}}`), &def); err != nil {
		t.Fatal(err)
	}
	r := &EphemeralRunnerReconciler{RunnerResources: def}
	q := func(l corev1.ResourceList, n corev1.ResourceName) string {
		v, ok := l[n]
		if !ok {
			return "unset"
		}
		return v.String()
	}

	pod := podFixture(t, r, "")
	c := pod.Spec.Containers[0]
	if q(c.Resources.Requests, corev1.ResourceCPU) != "1" || q(c.Resources.Limits, corev1.ResourceCPU) != "unset" {
		t.Fatalf("cpu = %v / %v", c.Resources.Requests, c.Resources.Limits)
	}
	if q(c.Resources.Requests, corev1.ResourceEphemeralStorage) != "4Gi" || q(c.Resources.Limits, corev1.ResourceEphemeralStorage) != "4Gi" {
		t.Fatalf("ephemeral-storage = %v / %v", c.Resources.Requests, c.Resources.Limits)
	}

	// A template naming a resource keeps it whole: an 8Gi request alone must not gain
	// the 4Gi default limit, which would put the limit below the request. Unnamed
	// resources still get defaults.
	pod = podFixture(t, r, `{"spec":{"containers":[{"name":"act-runner","resources":{
	  "requests":{"ephemeral-storage":"8Gi"},
	  "limits":{"memory":"6Gi"}}}]}}`)
	c = pod.Spec.Containers[0]
	if q(c.Resources.Requests, corev1.ResourceEphemeralStorage) != "8Gi" || q(c.Resources.Limits, corev1.ResourceEphemeralStorage) != "unset" {
		t.Fatalf("ephemeral-storage = %v / %v, want the template's alone", c.Resources.Requests, c.Resources.Limits)
	}
	if q(c.Resources.Limits, corev1.ResourceMemory) != "6Gi" || q(c.Resources.Requests, corev1.ResourceMemory) != "unset" {
		t.Fatalf("memory = %v / %v, want the template's alone", c.Resources.Requests, c.Resources.Limits)
	}
	if q(c.Resources.Requests, corev1.ResourceCPU) != "1" {
		t.Fatalf("cpu request = %s, want the default", q(c.Resources.Requests, corev1.ResourceCPU))
	}

	// Defaults must not leak between pods through shared maps.
	c.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("9")
	if got := r.RunnerResources.Requests[corev1.ResourceCPU]; got.String() != "1" {
		t.Fatalf("default mutated through a pod: cpu = %s", got.String())
	}
}
