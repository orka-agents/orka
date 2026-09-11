{{- define "orka.validateSubstrateCredentials" -}}
{{- $cfg := .Values.controller.substrate -}}
{{- $auth := $cfg.apiCredentials | default dict -}}
{{- if $auth.existingSecret -}}
{{- if or $cfg.apiCertFile $cfg.apiKeyFile $cfg.apiBearerTokenFile $cfg.apiCAFile -}}
{{- fail "controller.substrate.apiCredentials cannot be combined with explicit API file paths" -}}
{{- end -}}
{{- if ne (empty $auth.certKey) (empty $auth.privateKeyKey) -}}
{{- fail "controller.substrate.apiCredentials requires both certKey and privateKeyKey for mTLS" -}}
{{- end -}}
{{- if eq (empty $auth.certKey) (empty $auth.bearerTokenKey) -}}
{{- fail "controller.substrate.apiCredentials requires exactly one authentication method: mTLS keys or bearerTokenKey" -}}
{{- end -}}
{{- if and (not $cfg.apiInsecureSkipVerify) (empty $auth.caKey) -}}
{{- fail "controller.substrate.apiCredentials.caKey is required for verified Substrate API TLS" -}}
{{- end -}}
{{- else -}}
{{- if or $auth.certKey $auth.privateKeyKey $auth.bearerTokenKey $auth.caKey -}}
{{- fail "controller.substrate.apiCredentials.existingSecret is required when Secret keys are selected" -}}
{{- end -}}
{{- if $cfg.enabled -}}
{{- if or (ne (empty $cfg.apiCertFile) (empty $cfg.apiKeyFile)) (eq (empty $cfg.apiCertFile) (empty $cfg.apiBearerTokenFile)) -}}
{{- fail "controller.substrate.enabled requires exactly one control authentication method; configure apiCredentials or mounted API file paths" -}}
{{- end -}}
{{- end -}}
{{- if and (or $cfg.enabled $cfg.apiCertFile $cfg.apiKeyFile $cfg.apiBearerTokenFile) (not $cfg.apiInsecureSkipVerify) (empty $cfg.apiCAFile) -}}
{{- fail "controller.substrate.apiCAFile is required for verified Substrate API TLS" -}}
{{- end -}}
{{- end -}}
{{- end -}}
