{{- define "orka.enforcementBundled" -}}
{{- if hasKey .Subcharts "enforcement" -}}true{{- end -}}
{{- end -}}

{{- define "orka.validateEnforcement" -}}
{{- $enforcement := .Values.enforcement | default dict -}}
{{- if and $enforcement.enabled (not (include "orka.enforcementBundled" .)) -}}
{{- fail "enforcement.enabled requires a locally verified Guardd chart bundled by helmify" -}}
{{- end -}}
{{- if and (include "orka.enforcementBundled" .) (not (hasKey (.Values.podSecurityContext | default dict) "fsGroup")) -}}
{{- fail "the bundled enforcement projection requires podSecurityContext.fsGroup for controller key access" -}}
{{- end -}}
{{- if include "orka.enforcementBundled" . -}}
{{- $installation := include "guardd.installationName" .Subcharts.enforcement -}}
{{- $retained := lookup "v1" "ConfigMap" .Release.Namespace (printf "%s-installation" $installation) -}}
{{- if and (or $enforcement.enabled $retained) (eq (include "orka.watchNamespace" .) .Release.Namespace) -}}
{{- fail "enabled or retained enforcement requires controller.watchNamespace to differ from the management release namespace" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "orka.validateExistingEnforcement" -}}
{{- $root := .root -}}
{{- $pod := dig "spec" "template" "spec" (dict) .controller -}}
{{- range (get $pod "containers" | default (list)) -}}
{{- if and (eq (default "" .name) "controller") (has "--enforcement-provider-registry-projected=true" (default (list) .args)) -}}
{{- if not (include "orka.enforcementBundled" $root) -}}
{{- fail "a controller with retained enforcement projections requires a compatible bundled enforcement chart" -}}
{{- end -}}
{{- $name := include "guardd.installationName" $root.Subcharts.enforcement -}}
{{- $directory := $root.Subcharts.enforcement.Values.controllerMountDirectory -}}
{{- $mountMatches := false -}}
{{- range (default (list) .volumeMounts) -}}
{{- if eq .name "enforcement-providers" -}}
{{- $mountMatches = and (eq .mountPath $directory) (default false .readOnly) (not (default "" .subPath)) (not (default "" .subPathExpr)) -}}
{{- end -}}
{{- end -}}
{{- $registryMatches := false -}}
{{- $identityMatches := false -}}
{{- range (get $pod "volumes" | default (list)) -}}
{{- if eq .name "enforcement-providers" -}}
{{- range (dig "projected" "sources" (list) .) -}}
{{- if eq (dig "configMap" "name" "" .) (printf "%s-registry" $name) -}}
{{- $registryMatches = true -}}
{{- end -}}
{{- if eq (dig "secret" "name" "" .) (printf "%s-controller-identity" $name) -}}
{{- $identityMatches = true -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if not (and $mountMatches $registryMatches $identityMatches (has (printf "--enforcement-provider-registry=%s/registry.json" $directory) .args)) -}}
{{- fail "the effective enforcement projection identity and directory are immutable while retained cleanup authority may exist" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
