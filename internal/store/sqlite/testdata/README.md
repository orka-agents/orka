# SQLite bootstrap fixtures

`schema/0e567418.sql` and `schema/b5265055.sql` are the frozen upstream fixtures
introduced by the exact-schema migration tests. Historical 150-, 162-, 166- and
158-object layouts are reconstructed using that frozen 149-object fixture and
the immutable `migrations/149-to-150.sql`, `150-to-162.sql`, `162-to-166.sql`
and `166-to-167.sql` transitions, never the current schema constructor. Their
pinned fingerprints are asserted before exercising upgrades.

`bootstrap-snapshot.sql` contains one synthetic execution-snapshot row. It
uses the unchanged pre-metadata snapshot format and a test-only 32-byte key
consisting of `0x42`. Its plaintext is `{"prompt":"retained encrypted prompt"}`,
bound to Task UID `fixture-task-uid`. It contains no real credentials or user
data. The controller bootstrap integration test loads this fixed ciphertext
into the frozen historical schema and verifies missing/wrong/right key behavior
through the production startup function in both managed Secret and key-file
modes.

The store upgrade tests additionally populate supported historical layouts with
synthetic results, transcripts, ordered events, messages, sealed connector
custody and native-session state using unchanged record formats. They compare
every preexisting application table before and after migration, authenticate the
retained ciphertext, and verify new writes and stable identity after reopening.
