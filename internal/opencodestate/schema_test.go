package opencodestate

// Explicit fixture from OpenCode 1.18.9, commit
// 4da7bb44c84e013fa53e9c5d02ac753d1435c81a, packages/core/src/database/schema.gen.ts.
// Keep it independent of the production column allowlist so schema drift is
// observable. The real binary creates the production destination schema.
const fixtureSchema = `
CREATE TABLE workspace (
 id text PRIMARY KEY, type text NOT NULL, name text DEFAULT '' NOT NULL,
 branch text, directory text, extra text, project_id text NOT NULL, time_used integer NOT NULL
);
CREATE TABLE data_migration (name text PRIMARY KEY, time_completed integer NOT NULL);
CREATE TABLE account_state (id integer PRIMARY KEY, active_account_id text, active_org_id text);
CREATE TABLE account (
 id text PRIMARY KEY, email text NOT NULL, url text NOT NULL, access_token text NOT NULL,
 refresh_token text NOT NULL, token_expiry integer, time_created integer NOT NULL, time_updated integer NOT NULL
);
CREATE TABLE control_account (
 email text NOT NULL, url text NOT NULL, access_token text NOT NULL, refresh_token text NOT NULL,
 token_expiry integer, active integer NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL,
 PRIMARY KEY(email, url)
);
CREATE TABLE credential (
 id text PRIMARY KEY, integration_id text, label text NOT NULL, value text NOT NULL,
 connector_id text, method_id text, active integer, time_created integer NOT NULL, time_updated integer NOT NULL
);
CREATE TABLE event_sequence (aggregate_id text PRIMARY KEY, seq integer NOT NULL, owner_id text);
CREATE TABLE event (
 id text PRIMARY KEY, aggregate_id text NOT NULL, seq integer NOT NULL, type text NOT NULL, data text NOT NULL
);
CREATE TABLE permission (
 id text PRIMARY KEY, project_id text NOT NULL, action text NOT NULL, resource text NOT NULL,
 time_created integer NOT NULL, time_updated integer NOT NULL
);
CREATE TABLE project_directory (
 project_id text NOT NULL, directory text NOT NULL, type text, strategy text,
 time_created integer NOT NULL, PRIMARY KEY(project_id, directory)
);
CREATE TABLE project (
 id text PRIMARY KEY, worktree text NOT NULL, vcs text, name text, icon_url text,
 icon_url_override text, icon_color text, time_created integer NOT NULL, time_updated integer NOT NULL,
 time_initialized integer, sandboxes text NOT NULL, commands text
);
CREATE TABLE message (
 id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL,
 data text NOT NULL, FOREIGN KEY(session_id) REFERENCES session(id) ON DELETE CASCADE
);
CREATE TABLE part (
 id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL, time_created integer NOT NULL,
 time_updated integer NOT NULL, data text NOT NULL, FOREIGN KEY(message_id) REFERENCES message(id) ON DELETE CASCADE
);
CREATE TABLE session_context_epoch (
 session_id text PRIMARY KEY, baseline text NOT NULL, snapshot text NOT NULL, baseline_seq integer NOT NULL
);
CREATE TABLE session_input (
 id text PRIMARY KEY, session_id text NOT NULL, prompt text NOT NULL, delivery text NOT NULL,
 admitted_seq integer NOT NULL, promoted_seq integer, time_created integer NOT NULL
);
CREATE TABLE session_message (
 id text PRIMARY KEY, session_id text NOT NULL, type text NOT NULL, seq integer NOT NULL,
 time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL
);
CREATE TABLE session (
 id text PRIMARY KEY, project_id text NOT NULL, workspace_id text, parent_id text, slug text NOT NULL,
 directory text NOT NULL, path text, title text NOT NULL, version text NOT NULL, share_url text,
 summary_additions integer, summary_deletions integer, summary_files integer, summary_diffs text, metadata text,
 cost real DEFAULT 0 NOT NULL, tokens_input integer DEFAULT 0 NOT NULL, tokens_output integer DEFAULT 0 NOT NULL,
 tokens_reasoning integer DEFAULT 0 NOT NULL, tokens_cache_read integer DEFAULT 0 NOT NULL,
 tokens_cache_write integer DEFAULT 0 NOT NULL, revert text, permission text, agent text, model text,
 time_created integer NOT NULL, time_updated integer NOT NULL, time_compacting integer, time_archived integer,
 FOREIGN KEY(project_id) REFERENCES project(id) ON DELETE CASCADE
);
CREATE TABLE todo (
 session_id text NOT NULL, content text NOT NULL, status text NOT NULL, priority text NOT NULL,
 position integer NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL,
 PRIMARY KEY(session_id, position)
);
CREATE TABLE session_share (
 session_id text PRIMARY KEY, id text NOT NULL, secret text NOT NULL, url text NOT NULL,
 time_created integer NOT NULL, time_updated integer NOT NULL
);
CREATE TABLE migration (id text PRIMARY KEY, time_completed integer NOT NULL);
CREATE INDEX message_session_time_created_id_idx ON message(session_id,time_created,id);
CREATE INDEX part_message_id_id_idx ON part(message_id,id);
CREATE INDEX part_session_idx ON part(session_id);
`
