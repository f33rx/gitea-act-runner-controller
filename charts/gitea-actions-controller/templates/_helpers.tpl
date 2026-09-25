{{/*
Manager RBAC rules, shared between the ClusterRole and per-namespace Role
templates (ADR 0011 Decision 2) so the two shapes cannot drift apart.
*/}}
{{- define "gitea-actions-controller.managerRules" -}}
- apiGroups: ["giteaactions.blackrabbitpursuits.com"]
  resources: ["ephemeralrunners"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: ["giteaactions.blackrabbitpursuits.com"]
  resources: ["ephemeralrunners/status"]
  verbs: ["get", "update", "patch"]
- apiGroups: ["giteaactions.blackrabbitpursuits.com"]
  resources: ["ephemeralrunners/finalizers"]
  verbs: ["update"]
- apiGroups: ["giteaactions.blackrabbitpursuits.com"]
  resources: ["gitearunnersets"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: ["giteaactions.blackrabbitpursuits.com"]
  resources: ["gitearunnersets/status"]
  verbs: ["get", "update", "patch"]
- apiGroups: ["giteaactions.blackrabbitpursuits.com"]
  resources: ["ephemeralrunnersets"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: ["giteaactions.blackrabbitpursuits.com"]
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
- apiGroups: ["giteaactions.blackrabbitpursuits.com"]
  resources: ["gitearunnersets"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["giteaactions.blackrabbitpursuits.com"]
  resources: ["ephemeralrunnersets"]
  verbs: ["get", "list", "watch", "create", "update", "patch"]
- apiGroups: ["giteaactions.blackrabbitpursuits.com"]
  resources: ["ephemeralrunnersets/status"]
  verbs: ["get", "update", "patch"]
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "list", "watch"]
{{- end -}}

{{/*
Effective watch-namespace list as a comma-joined string (templates cannot return
lists; callers splitList ","). Always starts with the release namespace, because the
teardown credential Secret and leader-election Lease live there and the manager must
be able to read them whatever rbac.watchNamespaces says. Entries are tpl-rendered and
de-duplicated.
*/}}
{{- define "gitea-actions-controller.watchNamespaces" -}}
{{- $list := list .Release.Namespace -}}
{{- range .Values.rbac.watchNamespaces -}}
{{- $ns := tpl . $ -}}
{{- if not (has $ns $list) -}}{{- $list = append $list $ns -}}{{- end -}}
{{- end -}}
{{- join "," $list -}}
{{- end -}}

{{/*
Value for the --watch-namespaces flag: empty under clusterScope (cache everything),
otherwise the effective list so the informer cache matches the Role grants.
*/}}
{{- define "gitea-actions-controller.watchNamespacesFlag" -}}
{{- if not .Values.rbac.clusterScope -}}{{ include "gitea-actions-controller.watchNamespaces" . }}{{- end -}}
{{- end -}}

{{/* Image reference: explicit tag if set, otherwise the chart's appVersion. */}}
{{- define "gitea-actions-controller.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}

{{- define "gitea-actions-controller.runnerImage" -}}
{{- if .Values.runner.image -}}
{{- .Values.runner.image -}}
{{- else if .Values.runner.imageRepository -}}
{{- printf "%s:%s" .Values.runner.imageRepository .Chart.AppVersion -}}
{{- end -}}
{{- end -}}
