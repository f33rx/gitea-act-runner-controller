package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The teardown credential is the org-write Gitea token used to deregister runners
// (ADR 0006). Both the finalizer and the sweep need it; this file is the one place
// that knows where it lives and how it is shaped.

// Defaults match config/manager and the e2e harness so a bare binary keeps working.
// #nosec G101 -- Kubernetes object names, not credential material.
const (
	DefaultTeardownSecretNamespace = "gitea-actions-controller"
	DefaultTeardownSecretName      = "gitea-teardown-credential"
)

// teardownTokenKey is the Secret data key holding the token.
const teardownTokenKey = "token"

// teardownCredentialOrDefault fills either half of an unset reference so a partially
// configured reconciler still resolves to a usable key.
func teardownCredentialOrDefault(key types.NamespacedName) types.NamespacedName {
	if key.Namespace == "" {
		key.Namespace = DefaultTeardownSecretNamespace
	}
	if key.Name == "" {
		key.Name = DefaultTeardownSecretName
	}
	return key
}

// readTeardownToken returns the org-write token from the Secret at key (defaults
// applied). A missing Secret or an empty token is an error: without it nothing can be
// deregistered, and callers requeue rather than proceed.
func readTeardownToken(ctx context.Context, c client.Reader, key types.NamespacedName) (string, error) {
	key = teardownCredentialOrDefault(key)
	secret := &corev1.Secret{}
	if err := c.Get(ctx, key, secret); err != nil {
		return "", fmt.Errorf("read teardown credential Secret %s: %w", key, err)
	}
	token := string(secret.Data[teardownTokenKey])
	if token == "" {
		return "", fmt.Errorf("teardown credential Secret %s has no %q key or it is empty", key, teardownTokenKey)
	}
	return token, nil
}
