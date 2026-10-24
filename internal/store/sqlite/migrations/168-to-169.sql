ALTER TABLE memory_proposals ADD COLUMN apply_operation_id TEXT NOT NULL DEFAULT '';
ALTER TABLE memory_proposals ADD COLUMN application_abandoned_by TEXT NOT NULL DEFAULT '';
ALTER TABLE memory_proposals ADD COLUMN application_abandoned_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE memory_proposals ADD COLUMN application_abandoned_at TIMESTAMP;
UPDATE memory_proposals AS p
SET apply_operation_id = 'legacy-apply-migrated-' || p.id
WHERE p.apply_operation_id = ''
  AND lower(trim(p.type)) = 'memory'
  AND lower(trim(p.status)) = 'applied'
  AND trim(p.applied_memory_id) <> ''
  AND p.reviewed_at IS NOT NULL AND p.applied_at IS NOT NULL
  AND p.applied_at >= p.reviewed_at
  AND EXISTS (
    SELECT 1 FROM memories AS m
    WHERE m.namespace = p.namespace AND m.id = p.applied_memory_id
      AND m.source = 'memory_proposal' AND m.source_proposal_id = p.id
      AND m.content = p.content
  );
CREATE TABLE IF NOT EXISTS legacy_memory_archive (
  namespace_uid       TEXT NOT NULL,
  namespace_name      TEXT NOT NULL,
  id                  TEXT NOT NULL,
  session_name        TEXT NOT NULL DEFAULT '',
  agent_name          TEXT NOT NULL DEFAULT '',
  task_name           TEXT NOT NULL DEFAULT '',
  parent_task         TEXT NOT NULL DEFAULT '',
  source              TEXT NOT NULL DEFAULT '',
  source_proposal_id  TEXT NOT NULL DEFAULT '',
  content             TEXT NOT NULL,
  tags_json           TEXT NOT NULL DEFAULT '[]',
  disabled            BOOLEAN NOT NULL DEFAULT FALSE,
  deleted             BOOLEAN NOT NULL DEFAULT FALSE,
  created_at          TIMESTAMP NOT NULL,
  updated_at          TIMESTAMP NOT NULL,
  last_recalled_at    TIMESTAMP,
  recalled_count      INTEGER NOT NULL DEFAULT 0,
  backend_uid         TEXT NOT NULL,
  authority_epoch     INTEGER NOT NULL,
  routing_epoch       INTEGER NOT NULL,
  archived_at         TIMESTAMP NOT NULL,
  PRIMARY KEY (namespace_uid, id)
);
CREATE INDEX IF NOT EXISTS idx_legacy_memory_archive_namespace
  ON legacy_memory_archive(namespace_name, namespace_uid, archived_at, id);
CREATE TABLE IF NOT EXISTS memory_legacy_fences (
  namespace_name  TEXT PRIMARY KEY,
  namespace_uid   TEXT NOT NULL UNIQUE,
  backend_uid     TEXT NOT NULL,
  authority_epoch INTEGER NOT NULL CHECK (authority_epoch > 0),
  routing_epoch   INTEGER NOT NULL CHECK (routing_epoch > 0),
  created_at      TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS controller_feature_heartbeats (
  instance_id       TEXT PRIMARY KEY,
  role              TEXT NOT NULL,
  feature_epoch     INTEGER NOT NULL CHECK (feature_epoch >= 0),
  last_heartbeat_at TIMESTAMP NOT NULL,
  expires_at        TIMESTAMP NOT NULL CHECK (expires_at > last_heartbeat_at)
);
CREATE INDEX IF NOT EXISTS idx_controller_feature_heartbeats_live
  ON controller_feature_heartbeats(expires_at, role, feature_epoch);
CREATE TABLE IF NOT EXISTS controller_feature_epoch_history (
  role          TEXT NOT NULL,
  feature_epoch INTEGER NOT NULL CHECK (feature_epoch >= 0),
  first_seen_at TIMESTAMP NOT NULL,
  last_seen_at  TIMESTAMP NOT NULL CHECK (last_seen_at >= first_seen_at),
  PRIMARY KEY (role, feature_epoch)
);
CREATE TABLE IF NOT EXISTS memory_backend_bindings (
  namespace_name          TEXT NOT NULL UNIQUE,
  namespace_uid           TEXT PRIMARY KEY,
  cluster_id              TEXT NOT NULL,
  mode                    TEXT NOT NULL CHECK (mode IN ('legacy','remote')),
  backend_uid             TEXT NOT NULL,
  backend_generation      INTEGER NOT NULL CHECK (backend_generation > 0),
  authority_epoch         INTEGER NOT NULL CHECK (authority_epoch > 0),
  routing_epoch           INTEGER NOT NULL CHECK (routing_epoch > 0),
  spec_digest             TEXT NOT NULL,
  endpoint_digest          TEXT NOT NULL,
  resolved_address_digest  TEXT NOT NULL,
  server_certificate_digest TEXT NOT NULL,
  secret_name              TEXT NOT NULL,
  secret_key              TEXT NOT NULL,
  secret_uid              TEXT NOT NULL,
  secret_resource_version TEXT NOT NULL,
  tenant_id               TEXT NOT NULL,
  store_name              TEXT NOT NULL,
  store_uuid              TEXT NOT NULL,
  ownership_claim         TEXT NOT NULL,
  capability_revision     TEXT NOT NULL,
  protocol                TEXT NOT NULL,
  state                   TEXT NOT NULL CHECK (state IN ('legacy','validating','accepting','draining','recovering','decommissioned','removed')),
  activation_epoch        INTEGER NOT NULL CHECK (activation_epoch > 0),
  minimum_feature_epoch   INTEGER NOT NULL CHECK (minimum_feature_epoch >= 0),
  validation_expires_at   TIMESTAMP NOT NULL,
  activated_at            TIMESTAMP NOT NULL,
  updated_at              TIMESTAMP NOT NULL,
  decommissioned_at       TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_memory_backend_bindings_backend
  ON memory_backend_bindings(backend_uid, state, namespace_uid);
CREATE TABLE IF NOT EXISTS remote_memory_catalog (
  namespace_uid          TEXT NOT NULL,
  id                     TEXT NOT NULL,
  namespace_name         TEXT NOT NULL,
  cluster_id             TEXT NOT NULL,
  backend_uid            TEXT NOT NULL,
  authority_epoch        INTEGER NOT NULL CHECK (authority_epoch > 0),
  routing_epoch          INTEGER NOT NULL CHECK (routing_epoch > 0),
  tenant_id              TEXT NOT NULL,
  store_uuid             TEXT NOT NULL,
  backend_memory_id      TEXT NOT NULL DEFAULT '',
  backend_version        TEXT NOT NULL DEFAULT '',
  generation             INTEGER NOT NULL DEFAULT 0 CHECK (generation >= 0),
  desired_generation     INTEGER NOT NULL CHECK (desired_generation >= generation),
  governance_revision    INTEGER NOT NULL DEFAULT 1 CHECK (governance_revision > 0),
  materialization_state  TEXT NOT NULL CHECK (materialization_state IN ('pending','active','deleted','diverged','lost','orphaned')),
  disabled               BOOLEAN NOT NULL DEFAULT FALSE,
  deleted                BOOLEAN NOT NULL DEFAULT FALSE,
  trust                  TEXT NOT NULL CHECK (trust IN ('untrusted','reviewed','trusted')),
  session_name           TEXT NOT NULL DEFAULT '',
  agent_name             TEXT NOT NULL DEFAULT '',
  task_name              TEXT NOT NULL DEFAULT '',
  parent_task            TEXT NOT NULL DEFAULT '',
  source                 TEXT NOT NULL DEFAULT '',
  source_proposal_id     TEXT NOT NULL DEFAULT '',
  tags_json              TEXT NOT NULL DEFAULT '[]',
  pending_session_name   TEXT NOT NULL DEFAULT '',
  pending_agent_name     TEXT NOT NULL DEFAULT '',
  pending_task_name      TEXT NOT NULL DEFAULT '',
  pending_parent_task    TEXT NOT NULL DEFAULT '',
  pending_source         TEXT NOT NULL DEFAULT '',
  pending_tags_json      TEXT NOT NULL DEFAULT '[]',
  content_digest         TEXT NOT NULL DEFAULT '',
  content_available      BOOLEAN NOT NULL DEFAULT FALSE,
  pending_operation_id   TEXT NOT NULL DEFAULT '',
  created_at             TIMESTAMP NOT NULL,
  updated_at             TIMESTAMP NOT NULL,
  last_recalled_at       TIMESTAMP,
  recalled_count         INTEGER NOT NULL DEFAULT 0 CHECK (recalled_count >= 0),
  PRIMARY KEY (namespace_uid, id),
  FOREIGN KEY (namespace_uid) REFERENCES memory_backend_bindings(namespace_uid) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_remote_memory_catalog_backend_id
  ON remote_memory_catalog(namespace_uid, backend_memory_id)
  WHERE backend_memory_id <> '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_remote_memory_catalog_source_proposal
  ON remote_memory_catalog(namespace_uid, source_proposal_id)
  WHERE source_proposal_id <> '';
CREATE INDEX IF NOT EXISTS idx_remote_memory_catalog_list
  ON remote_memory_catalog(namespace_uid, deleted, disabled, updated_at DESC, id DESC);
CREATE TABLE IF NOT EXISTS remote_memory_generation_watermarks (
  namespace_uid   TEXT NOT NULL,
  id              TEXT NOT NULL,
  backend_uid     TEXT NOT NULL,
  authority_epoch INTEGER NOT NULL CHECK (authority_epoch > 0),
  routing_epoch   INTEGER NOT NULL CHECK (routing_epoch > 0),
  store_uuid      TEXT NOT NULL,
  generation      INTEGER NOT NULL CHECK (generation > 0),
  updated_at      TIMESTAMP NOT NULL,
  PRIMARY KEY (namespace_uid, id)
);
CREATE TABLE IF NOT EXISTS memory_operations (
  sequence                         INTEGER PRIMARY KEY AUTOINCREMENT,
  id                               TEXT NOT NULL UNIQUE,
  namespace_name                   TEXT NOT NULL,
  namespace_uid                    TEXT NOT NULL,
  cluster_id                       TEXT NOT NULL,
  backend_uid                      TEXT NOT NULL,
  authority_epoch                  INTEGER NOT NULL CHECK (authority_epoch > 0),
  routing_epoch                    INTEGER NOT NULL CHECK (routing_epoch > 0),
  memory_id                        TEXT NOT NULL,
  proposal_id                      TEXT NOT NULL DEFAULT '',
  kind                             TEXT NOT NULL CHECK (kind IN ('create','replace','delete')),
  desired_generation               INTEGER NOT NULL CHECK (desired_generation > 0),
  expected_materialized_generation INTEGER NOT NULL CHECK (expected_materialized_generation >= 0),
  expected_backend_version         TEXT NOT NULL DEFAULT '',
  operation_idempotency_key        TEXT NOT NULL,
  mutation_idempotency_key         TEXT NOT NULL,
  request_digest                   TEXT NOT NULL,
  mutation_digest                  TEXT NOT NULL,
  content_digest                   TEXT NOT NULL DEFAULT '',
  payload                          BLOB NOT NULL CHECK (length(payload) <= 524288),
  payload_bytes                    INTEGER NOT NULL CHECK (payload_bytes >= 0),
  state                            TEXT NOT NULL CHECK (state IN ('queued','leased','dispatching','ambiguous','dead_lettered','succeeded','abandoned','superseded','orphaned')),
  lease_owner                      TEXT NOT NULL DEFAULT '',
  lease_epoch                      INTEGER NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
  lease_origin_state               TEXT NOT NULL DEFAULT '',
  lease_expires_at                 TIMESTAMP,
  send_started_at                  TIMESTAMP,
  request_deadline                 TIMESTAMP,
  dispatches                       INTEGER NOT NULL DEFAULT 0 CHECK (dispatches >= 0),
  attempts                         INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  next_retry_at                    TIMESTAMP NOT NULL,
  max_age_at                       TIMESTAMP NOT NULL,
  payload_retain_until             TIMESTAMP NOT NULL,
  error_code                       TEXT NOT NULL DEFAULT '',
  error_message                    TEXT NOT NULL DEFAULT '',
  receipt_binding_digest           TEXT NOT NULL DEFAULT '',
  receipt_applied_generation       INTEGER NOT NULL DEFAULT 0,
  receipt_backend_version          TEXT NOT NULL DEFAULT '',
  receipt_backend_memory_id        TEXT NOT NULL DEFAULT '',
  receipt_content_digest           TEXT NOT NULL DEFAULT '',
  receipt_mutation_digest          TEXT NOT NULL DEFAULT '',
  receipt_completed_at             TIMESTAMP,
  actor                            TEXT NOT NULL,
  reason                           TEXT NOT NULL DEFAULT '',
  created_at                       TIMESTAMP NOT NULL,
  updated_at                       TIMESTAMP NOT NULL,
  completed_at                     TIMESTAMP,
  FOREIGN KEY (namespace_uid, memory_id) REFERENCES remote_memory_catalog(namespace_uid, id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_operations_unresolved_memory
  ON memory_operations(namespace_uid, memory_id)
  WHERE state IN ('queued','leased','dispatching','ambiguous','dead_lettered');
CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_operations_operation_idempotency
  ON memory_operations(namespace_uid, operation_idempotency_key);
CREATE INDEX IF NOT EXISTS idx_memory_operations_claim
  ON memory_operations(namespace_uid, backend_uid, authority_epoch, routing_epoch, state, next_retry_at, sequence);
CREATE INDEX IF NOT EXISTS idx_memory_operations_proposal
  ON memory_operations(namespace_uid, proposal_id, sequence DESC)
  WHERE proposal_id <> '';
CREATE TABLE IF NOT EXISTS memory_idempotency (
  namespace_uid           TEXT NOT NULL,
  principal               TEXT NOT NULL,
  route                   TEXT NOT NULL,
  caller_key              TEXT NOT NULL,
  request_digest          TEXT NOT NULL,
  authority_epoch         INTEGER NOT NULL,
  routing_epoch           INTEGER NOT NULL,
  original_status         INTEGER NOT NULL,
  response_type           TEXT NOT NULL CHECK (response_type IN ('memory','operation','empty')),
  memory_id               TEXT NOT NULL DEFAULT '',
  operation_id            TEXT NOT NULL DEFAULT '',
  location                TEXT NOT NULL DEFAULT '',
  retry_after_seconds     INTEGER NOT NULL DEFAULT 0 CHECK (retry_after_seconds >= 0),
  response_digest         TEXT NOT NULL DEFAULT '',
  response_snapshot       BLOB NOT NULL DEFAULT X'' CHECK (length(response_snapshot) <= 524288),
  expires_at              TIMESTAMP NOT NULL,
  terminal_binding_digest TEXT NOT NULL DEFAULT '',
  created_at              TIMESTAMP NOT NULL,
  updated_at              TIMESTAMP NOT NULL,
  PRIMARY KEY (namespace_uid, principal, route, caller_key)
);
CREATE INDEX IF NOT EXISTS idx_memory_idempotency_expiry
  ON memory_idempotency(expires_at, namespace_uid);
CREATE TABLE IF NOT EXISTS memory_search_cursors (
  id             TEXT PRIMARY KEY,
  namespace_uid  TEXT NOT NULL,
  binding_digest TEXT NOT NULL,
  query_digest   TEXT NOT NULL,
  state_json     BLOB NOT NULL,
  expires_at     TIMESTAMP NOT NULL,
  created_at     TIMESTAMP NOT NULL,
  retired_at     TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_memory_search_cursors_expiry
  ON memory_search_cursors(expires_at, namespace_uid);
CREATE INDEX IF NOT EXISTS idx_memory_search_cursors_active
  ON memory_search_cursors(namespace_uid, retired_at, expires_at);
CREATE TABLE IF NOT EXISTS memory_audit (
  id                     TEXT PRIMARY KEY,
  namespace_name         TEXT NOT NULL,
  namespace_uid          TEXT NOT NULL,
  actor                  TEXT NOT NULL,
  action                 TEXT NOT NULL,
  reason                 TEXT NOT NULL DEFAULT '',
  previous_state         TEXT NOT NULL DEFAULT '',
  new_state              TEXT NOT NULL DEFAULT '',
  authority_epoch        INTEGER NOT NULL DEFAULT 0,
  previous_routing_epoch INTEGER NOT NULL DEFAULT 0,
  routing_epoch          INTEGER NOT NULL DEFAULT 0,
  memory_id              TEXT NOT NULL DEFAULT '',
  operation_id           TEXT NOT NULL DEFAULT '',
  proposal_id            TEXT NOT NULL DEFAULT '',
  request_digest         TEXT NOT NULL DEFAULT '',
  mutation_digest        TEXT NOT NULL DEFAULT '',
  content_digest         TEXT NOT NULL DEFAULT '',
  request_id             TEXT NOT NULL DEFAULT '',
  created_at             TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memory_audit_namespace
  ON memory_audit(namespace_uid, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_memory_audit_memory
  ON memory_audit(namespace_uid, memory_id, created_at DESC)
  WHERE memory_id <> '';
CREATE TABLE IF NOT EXISTS memory_force_orphan_fences (
  namespace_uid    TEXT PRIMARY KEY,
  namespace_name   TEXT NOT NULL,
  backend_uid      TEXT NOT NULL,
  authority_epoch  INTEGER NOT NULL,
  routing_epoch    INTEGER NOT NULL,
  tenant_id        TEXT NOT NULL,
  store_uuid       TEXT NOT NULL,
  actor            TEXT NOT NULL,
  reason           TEXT NOT NULL,
  request_id       TEXT NOT NULL DEFAULT '',
  created_at       TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS memory_restore_previews (
  namespace_uid    TEXT PRIMARY KEY,
  backend_uid      TEXT NOT NULL,
  authority_epoch  INTEGER NOT NULL,
  routing_epoch    INTEGER NOT NULL,
  tenant_id        TEXT NOT NULL,
  store_name       TEXT NOT NULL,
  store_uuid       TEXT NOT NULL,
  preview_digest   TEXT NOT NULL,
  created_at       TIMESTAMP NOT NULL,
  expires_at       TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS memory_activation_recovery_receipts (
  namespace_uid  TEXT PRIMARY KEY,
  receipt_id     TEXT NOT NULL UNIQUE,
  namespace_name TEXT NOT NULL,
  backend_uid    TEXT NOT NULL,
  route_digest   TEXT NOT NULL,
  store_uuid     TEXT NOT NULL,
  manifest_digest TEXT NOT NULL,
  actor          TEXT NOT NULL,
  reason         TEXT NOT NULL,
  request_id     TEXT NOT NULL DEFAULT '',
  verified_at    TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS memory_verified_checkpoints (
  namespace_uid             TEXT PRIMARY KEY,
  checkpoint_id             TEXT NOT NULL UNIQUE,
  namespace_name            TEXT NOT NULL,
  backend_uid               TEXT NOT NULL,
  authority_epoch           INTEGER NOT NULL,
  routing_epoch             INTEGER NOT NULL,
  tenant_id                 TEXT NOT NULL,
  store_uuid                TEXT NOT NULL,
  maximum_operation_sequence INTEGER NOT NULL CHECK (maximum_operation_sequence >= 0),
  checkpoint_digest         TEXT NOT NULL,
  actor                     TEXT NOT NULL,
  reason                    TEXT NOT NULL,
  request_id                TEXT NOT NULL DEFAULT '',
  verified_at               TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS memory_audit_purge_guard (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 1)
);
CREATE TABLE IF NOT EXISTS memory_governance_purge_watermarks (
  namespace_uid                TEXT PRIMARY KEY,
  purge_count                  INTEGER NOT NULL CHECK (purge_count > 0),
  maximum_operation_sequence   INTEGER NOT NULL CHECK (maximum_operation_sequence >= 0),
  before_at                    TIMESTAMP NOT NULL,
  cumulative_payloads_purged   INTEGER NOT NULL,
  cumulative_receipts_purged   INTEGER NOT NULL,
  cumulative_idempotency_purged INTEGER NOT NULL,
  cumulative_tombstones_purged INTEGER NOT NULL,
  cumulative_audit_rows_purged INTEGER NOT NULL,
  purge_digest                 TEXT NOT NULL,
  actor                       TEXT NOT NULL,
  reason                      TEXT NOT NULL,
  request_id                  TEXT NOT NULL DEFAULT '',
  updated_at                  TIMESTAMP NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_proposals_apply_operation
  ON memory_proposals(namespace, apply_operation_id)
  WHERE apply_operation_id <> '';
CREATE TRIGGER IF NOT EXISTS trg_memories_fence_insert
  BEFORE INSERT ON memories
  WHEN EXISTS (SELECT 1 FROM memory_legacy_fences WHERE namespace_name = NEW.namespace)
  BEGIN SELECT RAISE(ABORT, 'legacy memory namespace fenced'); END;
CREATE TRIGGER IF NOT EXISTS trg_memories_fence_update
  BEFORE UPDATE ON memories
  WHEN EXISTS (SELECT 1 FROM memory_legacy_fences WHERE namespace_name IN (OLD.namespace, NEW.namespace))
  BEGIN SELECT RAISE(ABORT, 'legacy memory namespace fenced'); END;
CREATE TRIGGER IF NOT EXISTS trg_memories_fence_delete
  BEFORE DELETE ON memories
  WHEN EXISTS (SELECT 1 FROM memory_legacy_fences WHERE namespace_name = OLD.namespace)
  BEGIN SELECT RAISE(ABORT, 'legacy memory namespace fenced'); END;
CREATE TRIGGER IF NOT EXISTS trg_memory_audit_immutable_update
  BEFORE UPDATE ON memory_audit
  BEGIN SELECT RAISE(ABORT, 'memory audit is immutable'); END;
CREATE TRIGGER IF NOT EXISTS trg_memory_audit_immutable_delete
  BEFORE DELETE ON memory_audit
  WHEN NOT EXISTS (SELECT 1 FROM memory_audit_purge_guard WHERE singleton = 1)
  BEGIN SELECT RAISE(ABORT, 'memory audit is immutable'); END;
