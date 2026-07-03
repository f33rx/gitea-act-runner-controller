{{/*
Manager RBAC rules, shared between the ClusterRole and per-namespace Role
templates (ADR 0011 Decision 2) so the two shapes cannot drift apart.
*/}}
{{- define "gitea-actions-controller.managerRules" -}}
- apiGroups: ["giteaactions.blackrabbit.dev"]
  resources: ["ephemeralrunners"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: ["giteaactions.blackrabbit.dev"]
  resources: ["ephemeralrunners/status"]
  verbs: ["get", "update", "patch"]
- apiGroups: ["giteaactions.blackrabbit.dev"]
  resources: ["ephemeralrunners/finalizers"]
  verbs: ["update"]
- apiGroups: ["giteaactions.blackrabbit.dev"]
  resources: ["gitearunnersets"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: ["giteaactions.blackrabbit.dev"]
  resources: ["gitearunnersets/status"]
  verbs: ["get", "update", "patch"]
- apiGroups: ["giteaactions.blackrabbit.dev"]
  resources: ["ephemeralrunnersets"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: ["giteaactions.blackrabbit.dev"]
  resources: ["ephemeralrunnersets/status"]
  verbs: ["get", "update", "patch"]
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: [""]
  resources: ["serviceaccounts"]
  verbs: ["get", "list", "watch"]
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]
{{- end -}}

{{/*
Leader-election RBAC rule (coordination.k8s.io leases), namespaced -- only needed
when manager.leaderElection.enabled is true, and only ever in the release namespace
itself (leases are namespaced; controller-runtime defaults LeaderElectionNamespace to
the manager's own namespace).
*/}}
{{- define "gitea-actions-controller.leaderElectionRules" -}}
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
{{- end -}}

{{/*
Listener RBAC rules (ADR 0007): read-only to GiteaRunnerSets/Secrets, read-write only
to EphemeralRunnerSet spec/status. Never touches pods/runners/credentials directly.
*/}}
{{- define "gitea-actions-controller.listenerRules" -}}
- apiGroups: ["giteaactions.blackrabbit.dev"]
  resources: ["gitearunnersets"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["giteaactions.blackrabbit.dev"]
  resources: ["ephemeralrunnersets"]
  verbs: ["get", "list", "watch", "create", "update", "patch"]
- apiGroups: ["giteaactions.blackrabbit.dev"]
  resources: ["ephemeralrunnersets/status"]
  verbs: ["get", "update", "patch"]
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "list", "watch"]
{{- end -}}
