package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestReadTeardownToken(t *testing.T) {
	scheme := newTestScheme(t)
	mkSecret := func(ns, name, token string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Data:       map[string][]byte{"token": []byte(token)},
		}
	}
	for _, tc := range []struct {
		name    string
		objs    []client.Object
		key     types.NamespacedName
		want    string
		wantErr bool
	}{
		{"zero key falls back to the defaults", []client.Object{mkSecret(DefaultTeardownSecretNamespace, DefaultTeardownSecretName, "dflt")}, types.NamespacedName{}, "dflt", false},
		{"explicit key is honoured", []client.Object{mkSecret("rel", "my-td", "custom")}, types.NamespacedName{Namespace: "rel", Name: "my-td"}, "custom", false},
		{"missing Secret is an error", nil, types.NamespacedName{Namespace: "rel", Name: "my-td"}, "", true},
		{"empty token is an error", []client.Object{mkSecret("rel", "my-td", "")}, types.NamespacedName{Namespace: "rel", Name: "my-td"}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objs...).Build()
			got, err := readTeardownToken(context.Background(), c, tc.key)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("token=%q want %q", got, tc.want)
			}
		})
	}
}
