{{- define "orka.validateRuntimeFeedback" -}}
{{- $cfg := .Values.controller.runtimeFeedback | default dict -}}
{{- if not (kindIs "map" $cfg) -}}
{{- fail "controller.runtimeFeedback must be an object" -}}
{{- end -}}
{{- if and (hasKey $cfg "enabled") (not (kindIs "bool" $cfg.enabled)) -}}
{{- fail "controller.runtimeFeedback.enabled must be a boolean" -}}
{{- end -}}
{{- if $cfg.enabled -}}
{{- if ne .Values.controller.mode "harness-v2" -}}
{{- fail "controller.runtimeFeedback.enabled requires controller.mode=harness-v2" -}}
{{- end -}}
{{- range $key := list "url" "nodeURLsKey" "existingSecret" "caKey" "certKey" "privateKeyKey" -}}
{{- $value := get $cfg $key -}}
{{- if not (kindIs "string" $value) -}}
{{- fail (printf "controller.runtimeFeedback.%s must be a string" $key) -}}
{{- end -}}
{{- if ne $value (trim $value) -}}
{{- fail (printf "controller.runtimeFeedback.%s must not contain surrounding whitespace" $key) -}}
{{- end -}}
{{- end -}}
{{- if eq (empty $cfg.url) (empty $cfg.nodeURLsKey) -}}
{{- fail "controller.runtimeFeedback.enabled requires exactly one url or nodeURLsKey" -}}
{{- end -}}
{{- range $key := list "existingSecret" "caKey" "certKey" "privateKeyKey" -}}
{{- if empty (get $cfg $key) -}}
{{- fail (printf "controller.runtimeFeedback.%s is required when enabled" $key) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
