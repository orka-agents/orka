#!/bin/sh
# This demo uses a text/function-tool policy model, not hosted Codex tools.
exec /opt/codex/bin/codex-native \
  -c 'model_catalog_json="/opt/orka-semantic-router/models.json"' \
  -c 'web_search="disabled"' \
  -c 'features.remote_compaction_v2=false' \
  -c 'features.code_mode=false' \
  -c 'features.code_mode_only=false' \
  -c 'features.multi_agent=false' \
  "$@"
