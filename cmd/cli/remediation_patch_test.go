package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/remediation/contract"
)

// These literal and delta fixtures were produced by git diff --no-index
// --binary from disposable synthetic files, not by the decoder under test.
const remediationLiteralPatch = `diff --git a/literal.bin b/literal.bin
index eaf36c1daccfdf325514461cd1a2ffbc139b5464..82ae0e33690b082386b8b25a3b10eba15c20285b 100644
GIT binary patch
literal 4
LcmZQzWMKvX01^NR

literal 4
LcmZQzWMT#Y01f~L

`

const remediationDeltaPatch = `diff --git a/delta.bin b/delta.bin
index c2532a76d98b3de3ef230a5c37888aa5e951ae5c..c154260f70f19bc7ca3a5ec4bb02c6076ce94bd8 100644
GIT binary patch
delta 15
WcmZo@U~Fh$+~6S3sI}2?zXAX$ECmh#

delta 16
XcmZo@U~Fh$+~6R;&_RBq!vRJBF~|kP

`

func remediationValidPatchFixtures() map[string]string {
	return map[string]string{
		"git-text": remediationSyntheticPatch,
		"plain-unified": "--- a/data.txt\t2026-01-01\n+++ b/data.txt\t2026-01-02\n" +
			"@@ -1,3 +1,3 @@ function context\n first\n-old\n+new\n last\n",
		"index-and-modes": "diff --git a/data.txt b/data.txt\nold mode 100644\nnew mode 100755\n" +
			"index abc1234..def1234\n--- a/data.txt\n+++ b/data.txt\n@@ -1 +1 @@\n-old\n+new\n",
		"regular-index": strings.Replace(remediationSyntheticPatch, "--- a/", "index abc1234..def1234 100644\n--- a/", 1),
		"new-file": "diff --git a/new.txt b/new.txt\nnew file mode 100644\nindex 0000000..abc1234\n" +
			"--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1,2 @@\n+first\n+second\n",
		"new-no-prefix": "diff --git b/new.txt b/new.txt\nnew file mode 100644\n" +
			"--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1 @@\n+first\n",
		"deleted-file": "diff --git a/old.txt b/old.txt\ndeleted file mode 100644\nindex abc1234..0000000\n" +
			"--- a/old.txt\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-first\n-second\n",
		"deleted-no-prefix": "diff --git a/old.txt a/old.txt\ndeleted file mode 100644\n" +
			"--- a/old.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-first\n",
		"mode-only":   "diff --git a/script b/script\nold mode 100644\nnew mode 100755\n",
		"empty-file":  "diff --git a/empty b/empty\nnew file mode 100644\nindex 0000000..e69de29\n",
		"rename-only": "diff --git a/old.txt b/new.txt\nsimilarity index 100%\nrename from old.txt\nrename to new.txt\n",
		"copy-only":   "diff --git a/old.txt b/copy.txt\nsimilarity index 100%\ncopy from old.txt\ncopy to copy.txt\n",
		"rename-and-hunk": "diff --git a/old.txt b/new.txt\nsimilarity index 80%\nrename from old.txt\nrename to new.txt\n" +
			"index abc1234..def1234 100644\n--- a/old.txt\n+++ b/new.txt\n@@ -1,3 +1,3 @@\n first\n-old\n+new\n last\n",
		"rename-no-prefix": "diff --git a/old.txt b/new.txt\nsimilarity index 80%\nrename from a/old.txt\nrename to b/new.txt\n" +
			"--- a/old.txt\n+++ b/new.txt\n@@ -1 +1 @@\n-old\n+new\n",
		"no-prefix": "diff --git data.txt data.txt\n--- data.txt\n+++ data.txt\n@@ -1 +1 @@\n-old\n+new\n",
		"space-path": "diff --git a/path with spaces b/path with spaces\n" +
			"--- a/path with spaces\n+++ b/path with spaces\n@@ -1 +1 @@\n-old\n+new\n",
		"quoted-path": "diff --git \"a/caf\\303\\251.txt\" \"b/caf\\303\\251.txt\"\n" +
			"--- \"a/caf\\303\\251.txt\"\n+++ \"b/caf\\303\\251.txt\"\n@@ -1 +1 @@\n-old\n+new\n",
		"multiple-hunks": "--- a/file\n+++ b/file\n@@ -1 +1 @@\n-old\n+new\n@@ -20,2 +20,3 @@ other\n" +
			" same\n-old\n+new\n+extra\n",
		"multiple-files": remediationSyntheticPatch + "--- a/other\n+++ b/other\n@@ -1 +1 @@\n-old\n+new\n",
		"newline-markers": "--- a/file\n+++ b/file\n@@ -1 +1 @@\n-old\n\\ No newline at end of file\n" +
			"+new\n\\ No newline at end of file\n",
		"empty-content-lines": "--- a/file\n+++ b/file\n@@ -1,2 +1,2 @@\n \n-old\n+\n",
		"literal":             remediationLiteralPatch,
		"delta":               remediationDeltaPatch,
		"binary-and-text":     remediationLiteralPatch + remediationSyntheticPatch,
	}
}

func TestRemediationPatchAcceptsCanonicalChanges(t *testing.T) {
	for name, fixture := range remediationValidPatchFixtures() {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, validateRemediationPatch([]byte(fixture)))
		})
	}
}

func TestRemediationPatchRejectsNonPatchesAndMalformedStructure(t *testing.T) {
	for name, fixture := range map[string]string{
		"html":              "<html>synthetic-private-response</html>\n",
		"json":              "{\"error\":\"synthetic-private-response\"}\n",
		"plain-text":        "synthetic-private-response\n",
		"git-prefix-only":   "diff --git a/file b/file\n",
		"headers-only":      "diff --git a/file b/file\n--- a/file\n+++ b/file\n",
		"mismatched-header": "diff --git a/other b/other\n--- a/file\n+++ b/file\n@@ -1 +1 @@\n-old\n+new\n",
		"empty-path":        "--- \n+++ b/file\n@@ -1 +1 @@\n-old\n+new\n",
		"null-paths":        "--- /dev/null\n+++ /dev/null\n@@ -0,0 +1 @@\n+new\n",
		"traversal":         "--- a/../file\n+++ b/../file\n@@ -1 +1 @@\n-old\n+new\n",
		"control-path":      "--- \"a/file\\nname\"\n+++ b/file\n@@ -1 +1 @@\n-old\n+new\n",
		"unknown-metadata":  "diff --git a/file b/file\nsynthetic-private-response\n--- a/file\n+++ b/file\n@@ -1 +1 @@\n-old\n+new\n",
		"incomplete-mode":   "diff --git a/file b/file\nold mode 100644\n",
		"unchanged-mode":    "diff --git a/file b/file\nold mode 100644\nnew mode 100644\n",
		"incomplete-rename": "diff --git a/file b/other\nrename from file\n",
		"fake-rename":       "diff --git a/file b/file\nsimilarity index 100%\nrename from file\nrename to file\n",
		"partial-rename":    "diff --git a/file b/other\nsimilarity index 90%\nrename from file\nrename to other\n",
		"duplicate-mode":    "diff --git a/file b/file\nold mode 100644\nold mode 100644\nnew mode 100755\n",
		"invalid-index":     "diff --git a/file b/file\nindex not-an-oid..also-invalid\n--- a/file\n+++ b/file\n@@ -1 +1 @@\n-old\n+new\n",
		"truncated-hunk":    "--- a/file\n+++ b/file\n@@ -1,2 +1,2 @@\n-old\n+new\n",
		"extra-lines":       "--- a/file\n+++ b/file\n@@ -1 +1 @@\n-old\n+new\n+extra\n",
		"no-changes":        "--- a/file\n+++ b/file\n@@ -1 +1 @@\n unchanged\n",
		"zero-counts":       "--- a/file\n+++ b/file\n@@ -0,0 +0,0 @@\n",
		"overflow-counts":   "--- a/file\n+++ b/file\n@@ -1,4294967296 +1 @@\n-old\n+new\n",
		"huge-counts":       "--- a/file\n+++ b/file\n@@ -1,262145 +1 @@\n-old\n+new\n",
		"backwards-hunks":   "--- a/file\n+++ b/file\n@@ -20 +20 @@\n-old\n+new\n@@ -10 +10 @@\n-old\n+new\n",
		"wrong-new-file":    "--- /dev/null\n+++ b/file\n@@ -1 +1 @@\n-old\n+new\n",
		"missing-newline":   strings.TrimSuffix(remediationSyntheticPatch, "\n"),
		"unbound-marker":    "--- a/file\n+++ b/file\n@@ -1 +1 @@\n\\ No newline at end of file\n-old\n+new\n",
		"early-marker":      "--- a/file\n+++ b/file\n@@ -1,2 +1,2 @@\n-old\n\\ No newline at end of file\n+new\n same\n",
		"duplicate-marker": "--- a/file\n+++ b/file\n@@ -1 +1 @@\n-old\n+new\n\\ No newline at end of file\n" +
			"\\ No newline at end of file\n",
		"hunk-after-eof": "--- a/file\n+++ b/file\n@@ -1 +1 @@\n-old\n+new\n\\ No newline at end of file\n" +
			"@@ -20 +20 @@\n-old\n+new\n",
		"html-tail":      remediationSyntheticPatch + "<html>synthetic-private-response</html>\n",
		"json-tail":      remediationSyntheticPatch + "{\"error\":\"synthetic-private-response\"}\n",
		"partial-second": remediationSyntheticPatch + "diff --git a/other b/other\n--- a/other\n+++ b/other\n",
		"binary-prefix":  "diff --git a/file b/file\nGIT binary patch\n",
		"binary-corrupt": strings.Replace(remediationLiteralPatch, "LcmZQzWMKvX01^NR", "A~~~~~", 1),
		"binary-size":    strings.Replace(remediationLiteralPatch, "literal 4", "literal 5", 1),
		"binary-budget":  strings.Replace(remediationLiteralPatch, "literal 4", "literal 4294967295", 1),
	} {
		t.Run(name, func(t *testing.T) {
			err := validateRemediationPatch([]byte(fixture))
			require.ErrorIs(t, err, errRemediationPatch)
			require.NotContains(t, err.Error(), "synthetic-private")
		})
	}
}

func TestRemediationPatchParserBounds(t *testing.T) {
	prefix := "--- a/file\n+++ b/file\n@@ -1 +1 @@\n-old\n+"
	fixture := prefix + strings.Repeat("x", remediationMaxPatchBytes-len(prefix)-1) + "\n"
	require.NoError(t, validateRemediationPatch([]byte(fixture)))
	require.ErrorIs(t, validateRemediationPatch([]byte(fixture+"\n")), errRemediationPatch)
	require.ErrorIs(t, validateRemediationPatch([]byte{}), errRemediationPatch)
	require.ErrorIs(t, validateRemediationPatch([]byte(remediationSyntheticPatch+"\x00\n")), errRemediationPatch)
	require.ErrorIs(t, validateRemediationPatch([]byte(remediationSyntheticPatch+"\xff\n")), errRemediationPatch)
	for _, delta := range [][]byte{
		{1, 1, 0}, {1, 2, 1, 'x'}, {1, 1, 2, 'x'}, {1, 1, 0x90, 2},
		{1, 1, 0x91, 1, 1}, {1, 1, 0x90}, {0xff, 0xff, 0xff, 0xff, 0x7f},
	} {
		require.False(t, remediationBinaryDelta(delta))
	}
	for _, test := range []struct {
		budget int
		valid  bool
	}{{7, false}, {8, true}} {
		parser := remediationPatchParser{lines: strings.Split(remediationLiteralPatch, "\n"), binaryBudget: test.budget}
		require.Equal(t, test.valid, parser.file())
	}
	parser := remediationPatchParser{
		lines: strings.Split(remediationLiteralPatch+remediationLiteralPatch, "\n"), binaryBudget: 15,
	}
	require.True(t, parser.file())
	parser.skipBlank()
	require.False(t, parser.file(), "binary expansion budget must span file sections")
}

func TestRemediationDownloadPreservesValidCandidatePatches(t *testing.T) {
	directory := remediationTestDirectory(t)
	token := remediationTestFile(t, directory, "token", "synthetic-private-token")
	for name, patch := range remediationValidPatchFixtures() {
		t.Run(name, func(t *testing.T) {
			for _, verified := range []bool{false, true} {
				phase, outcome, label := contract.PhaseFailed, contract.OutcomeUnverified, "Candidate patch saved (not verified).\n"
				if verified {
					phase, outcome, label = contract.PhaseSucceeded, contract.OutcomeVerified, "Patch saved (verified checks; evidence scope unrecorded).\n"
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/patch") {
						w.Header().Set("Content-Type", "application/octet-stream")
						_, _ = w.Write([]byte(patch))
						return
					}
					remediationReply(w, http.StatusOK, "request-one", phase, contract.StageComplete, outcome, true)
				}))
				defer server.Close()
				outputPath := filepath.Join(directory, name+"-"+outcome+".patch")
				_, diagnostic, err := remediationCLI(t, "download", "request-one", "--output", outputPath,
					"--server", server.URL, "--namespace", "testing", "--token-file", token)
				require.NoError(t, err)
				require.Equal(t, label, diagnostic)
				content, err := os.ReadFile(outputPath)
				require.NoError(t, err)
				require.Equal(t, patch, string(content))
			}
		})
	}
}
