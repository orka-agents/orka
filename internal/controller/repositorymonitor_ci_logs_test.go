package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/orka-agents/orka/internal/store"
)

func TestRepositoryMonitorCILogExtraction(t *testing.T) {
	const ts = "2026-11-16T10:00:00.1234567Z "
	errorLog := strings.Join([]string{
		ts + "##[group]Run make test",
		ts + "make test",
		ts + "##[endgroup]",
		ts + "\x1b[31m--- FAIL: TestThing (0.01s)\x1b[0m",
		ts + "    thing_test.go:12: got 1, want 2",
		ts + "##[error]Process completed with exit code 2.",
		ts + "Post job cleanup.",
		ts + "##[error]cache save failed",
		ts + "Cleaning up orphan processes",
	}, "\r\n")
	tailLog := strings.Join([]string{
		ts + "##[group]Run make lint",
		ts + "lint found 3 issues",
		ts + "exit status 1",
		ts + "Post job cleanup.",
		ts + "/usr/bin/git version",
		ts + "Cleaning up orphan processes",
	}, "\n")
	huge := ts + "panic: " + strings.Repeat("x", 3*repositoryMonitorCILogRawLineLimit) + "\n" + ts + "##[error]boom\n"
	var long strings.Builder
	for i := range 2000 {
		fmt.Fprintf(&long, "%sline %d\n", ts, i)
		if i == 100 {
			long.WriteString(ts + "##[error]early failure\n")
		}
	}
	// A credential wrapped with backslash continuations whose recognizable
	// prefix line is just outside the retained window.
	frags := repositoryMonitorCILogTestFragments(40)
	// lead starts each wrapped line; an escaped break continues a
	// credential into any character, such as a URL escape.
	forwardLog := func(lead string) string {
		var forward strings.Builder
		for range 60 {
			forward.WriteString(ts + "filler\n")
		}
		forward.WriteString(ts + "export GITHUB_TOKEN=gh" + "p_" + frags[0] + "\\\n")
		for _, frag := range frags[1:30] {
			forward.WriteString(ts + lead + frag + "\\\n")
		}
		for _, frag := range frags[30:39] {
			forward.WriteString(ts + lead + frag + " \\\n")
		}
		forward.WriteString(ts + lead + frags[39] + "\n")
		for i := range 8 {
			fmt.Fprintf(&forward, "%s[step] output %d\n", ts, i)
		}
		forward.WriteString(ts + "##[error]Process completed with exit code 1.\n")
		return forward.String()
	}
	// The tail window starts at a line that is only a backslash.
	var standalone strings.Builder
	standalone.WriteString(ts + "export GITHUB_TOKEN=gh" + "p_" + frags[0] + "\\\n" + ts + "\\\n")
	for _, frag := range frags[1:22] {
		standalone.WriteString(ts + frag + "\\\n")
	}
	standalone.WriteString(ts + frags[22] + "\n")
	for i := range 26 {
		fmt.Fprintf(&standalone, "%s[step] output %d\n", ts, i)
	}
	// A credential wrapped into the part of the log the read limit left
	// unread: ending in a backslash word, and in single-word lines.
	reverse := strings.Repeat(ts+"build output (step)\n", 20)
	for _, frag := range frags[:5] {
		reverse += ts + frag + "\\\n"
	}
	reverseWord := reverse + ts + frags[5] + " \\\n"
	reverseSingle := reverse + ts + frags[5] + "\n"
	fragmentsFrom := func(from, to int) []string { return frags[from:to] }
	pemLog := func(bodyLines int) string {
		lines := []string{ts + "-----" + "BEGIN RSA PRIVATE KEY-----"}
		for _, line := range repositoryMonitorEvidenceTestPEMBody(bodyLines) {
			lines = append(lines, ts+line)
		}
		return strings.Join(append(lines, ts+"-----"+"END RSA PRIVATE KEY-----", ts+"##[error]key rejected"), "\n") + "\n"
	}
	for _, tc := range []struct {
		name          string
		log           string
		limit         int64
		wantKind      string
		wantTruncated bool
		want          []string
		notWant       []string
	}{
		{name: "error marker", log: errorLog, limit: repositoryMonitorCILogReadLimit, wantKind: repositoryMonitorCILogExcerptLastError,
			want:    []string{"Run make test", "--- FAIL: TestThing (0.01s)", "thing_test.go:12: got 1, want 2", "##[error]Process completed with exit code 2."},
			notWant: []string{"2026-11-16T", "\x1b", "\r", "##[endgroup]", "##[group]", "cache save failed", "Cleaning up orphan processes"}},
		{name: "no marker falls back to the tail before cleanup", log: tailLog, limit: repositoryMonitorCILogReadLimit, wantKind: repositoryMonitorCILogExcerptTail,
			want: []string{"lint found 3 issues", "exit status 1"}, notWant: []string{"Post job cleanup", "git version", "orphan"}},
		{name: "only a cleanup error", log: ts + "ok\n" + ts + "Post job cleanup.\n" + ts + "##[error]cache save failed\n", limit: repositoryMonitorCILogReadLimit, wantKind: repositoryMonitorCILogExcerptTail,
			want: []string{"ok"}, notWant: []string{"cache save failed"}},
		// A credential whose recognizable start falls past the raw line
		// limit, with its body wrapped onto the next line.
		{name: "credential cut at the raw line limit", log: ts + "dump " + strings.Repeat("ab ", repositoryMonitorCILogRawLineLimit/3) + "token=gh" + "p_" + repositoryMonitorEvidenceTestGitHubToken[4:20] + "\n" + ts + repositoryMonitorEvidenceTestGitHubToken[20:] + "\n" + ts + "##[error]failed\n",
			limit: repositoryMonitorCILogReadLimit, wantKind: repositoryMonitorCILogExcerptLastError, want: []string{"dump ab", "##[error]failed"}, notWant: []string{repositoryMonitorEvidenceTestGitHubToken[20:], repositoryMonitorEvidenceTestGitHubToken[4:20]}},
		{name: "huge single line", log: huge, limit: repositoryMonitorCILogReadLimit, wantKind: repositoryMonitorCILogExcerptLastError, want: []string{"panic: …", "##[error]boom"}, notWant: []string{"xxx"}},
		{name: "empty", log: "", limit: repositoryMonitorCILogReadLimit, wantKind: repositoryMonitorCILogExcerptTail, notWant: []string{repositoryMonitorEvidenceRedactedLine}},
		{name: "only group markers", log: ts + "##[group]\n" + ts + "##[endgroup]\n", limit: repositoryMonitorCILogReadLimit, wantKind: repositoryMonitorCILogExcerptTail,
			notWant: []string{repositoryMonitorEvidenceRedactedLine}},
		{name: "progress output before a failure", log: strings.Join([]string{
			ts + "\x1b[2KDownloading: 10%\rDownloading: 50%\r\x1b[2KDownloading: 100%",
			ts + "--- FAIL: TestThing (0.01s)",
			ts + "    thing_test.go:12: got 1, want 2",
			ts + "##[error]Process completed with exit code 1.",
		}, "\n"), limit: repositoryMonitorCILogReadLimit, wantKind: repositoryMonitorCILogExcerptLastError,
			want:    []string{"Downloading: 100%", "--- FAIL: TestThing (0.01s)", "thing_test.go:12: got 1, want 2", "##[error]Process completed with exit code 1."},
			notWant: []string{"10%", "50%", "\r", repositoryMonitorEvidenceRedactedLine}},
		{name: "backslash-wrapped secret entering the window", log: forwardLog(""), limit: repositoryMonitorCILogReadLimit, wantKind: repositoryMonitorCILogExcerptLastError,
			want: []string{"[step] output 7", "##[error]Process completed"}, notWant: fragmentsFrom(1, 40)},
		{name: "backslash-wrapped secret entering the window at punctuation", log: forwardLog("%2F"), limit: repositoryMonitorCILogReadLimit, wantKind: repositoryMonitorCILogExcerptLastError,
			want: []string{"[step] output 7", "##[error]Process completed"}, notWant: fragmentsFrom(1, 40)},
		{name: "backslash-wrapped secret through a backslash line", log: standalone.String(), limit: repositoryMonitorCILogReadLimit, wantKind: repositoryMonitorCILogExcerptTail,
			want: []string{"[step] output 25"}, notWant: fragmentsFrom(1, 23)},
		{name: "backslash-wrapped secret leaving the read portion", log: reverseWord + strings.Repeat(ts+"rest\n", 10), limit: int64(len(reverseWord)), wantKind: repositoryMonitorCILogExcerptTailOfReadPart,
			wantTruncated: true, want: []string{"build output (step)"}, notWant: fragmentsFrom(0, 6)},
		{name: "single-word lines after backslash-wrapped lines", log: reverseSingle + strings.Repeat(ts+"rest\n", 10), limit: int64(len(reverseSingle)), wantKind: repositoryMonitorCILogExcerptTailOfReadPart,
			wantTruncated: true, want: []string{"build output (step)"}, notWant: fragmentsFrom(0, 6)},
		{name: "truncated read does not claim the last error", log: long.String(), limit: 4 << 10, wantKind: repositoryMonitorCILogExcerptErrorInReadPart, wantTruncated: true, want: []string{"##[error]early failure"}},
		{name: "truncated read without an error", log: strings.Repeat(ts+"build output (step)\n", 1000), limit: 1 << 10, wantKind: repositoryMonitorCILogExcerptTailOfReadPart, wantTruncated: true, want: []string{"build output (step)"}},
		{name: "single-word lines at an open edge are withheld", log: strings.Repeat(ts+"QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo=\n", 1000), limit: 1 << 10, wantKind: repositoryMonitorCILogExcerptTailOfReadPart, wantTruncated: true, notWant: []string{"QUJD"}},
		{name: "secret across the excerpt edge", log: strings.Repeat(ts+"filler\n", 60) + ts + repositoryMonitorEvidenceTestGitHubToken[:20] + "\n" + ts + repositoryMonitorEvidenceTestGitHubToken[20:] + "\n" + ts + "##[error]failed\n",
			limit: repositoryMonitorCILogReadLimit, wantKind: repositoryMonitorCILogExcerptLastError, want: []string{"##[error]failed"}, notWant: []string{repositoryMonitorEvidenceTestGitHubToken[20:], repositoryMonitorEvidenceTestGitHubToken[4:20]}},
		{name: "pem block before the error", log: ts + "loading key\n" + pemLog(20), limit: repositoryMonitorCILogReadLimit, wantKind: repositoryMonitorCILogExcerptLastError,
			want: []string{"loading key", "##[error]key rejected"}, notWant: []string{"Q3xR9+/w", "PRIVATE KEY"}},
		// The block's BEGIN line is outside the retained window.
		{name: "excerpt starting inside a pem block", log: pemLog(80), limit: repositoryMonitorCILogReadLimit, wantKind: repositoryMonitorCILogExcerptLastError,
			want: []string{"##[error]key rejected"}, notWant: []string{"Q3xR9+/w", "PRIVATE KEY"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scanner, truncated, err := readRepositoryMonitorCILog(strings.NewReader(tc.log), tc.limit)
			if err != nil || truncated != tc.wantTruncated {
				t.Fatalf("readRepositoryMonitorCILog() truncated=%t err=%v, want truncated=%t", truncated, err, tc.wantTruncated)
			}
			excerpt, kind := scanner.excerpt(truncated)
			text := repositoryMonitorCILogExcerptText(excerpt)
			if kind != tc.wantKind {
				t.Fatalf("excerpt kind = %q, want %q (text %q)", kind, tc.wantKind, text)
			}
			if len(text) > repositoryMonitorCILogExcerptBytes || strings.Count(text, "\n")+1 > repositoryMonitorCILogExcerptLines {
				t.Fatalf("excerpt is %d bytes and %d lines", len(text), strings.Count(text, "\n")+1)
			}
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Fatalf("excerpt = %q, want %q", text, want)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(text, notWant) {
					t.Fatalf("excerpt = %q, want no %q", text, notWant)
				}
			}
		})
	}
}

// repositoryMonitorCILogTestFragments returns n distinct base62 fragments
// of a synthetic wrapped credential.
func repositoryMonitorCILogTestFragments(n int) []string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	fragments := make([]string, n)
	for i := range fragments {
		var fragment strings.Builder
		for j := range 12 {
			fragment.WriteByte(alphabet[(i*7+j*11+3)%len(alphabet)])
		}
		fragments[i] = fragment.String()
	}
	return fragments
}

// TestRepositoryMonitorCILogReadNeedsEOFAfterTheLimit covers a read that
// reached the limit: only an EOF from the probe past it marks the log
// complete, and a failed probe marks it truncated.
func TestRepositoryMonitorCILogReadNeedsEOFAfterTheLimit(t *testing.T) {
	const log = "2026-11-16T10:00:00Z ##[error]Process completed with exit code 1.\n"
	for _, tc := range []struct {
		name          string
		after         io.Reader
		wantTruncated bool
		wantKind      string
	}{
		{name: "eof", after: strings.NewReader(""), wantKind: repositoryMonitorCILogExcerptLastError},
		{name: "connection reset", after: iotest.ErrReader(errors.New("connection reset by peer")), wantTruncated: true, wantKind: repositoryMonitorCILogExcerptErrorInReadPart},
		{name: "timeout", after: iotest.ErrReader(context.DeadlineExceeded), wantTruncated: true, wantKind: repositoryMonitorCILogExcerptErrorInReadPart},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scanner, truncated, err := readRepositoryMonitorCILog(io.MultiReader(strings.NewReader(log), tc.after), int64(len(log)))
			if err != nil || truncated != tc.wantTruncated {
				t.Fatalf("readRepositoryMonitorCILog() truncated=%t err=%v, want truncated=%t", truncated, err, tc.wantTruncated)
			}
			if _, kind := scanner.excerpt(truncated); kind != tc.wantKind {
				t.Fatalf("excerpt kind = %q, want %q", kind, tc.wantKind)
			}
		})
	}
}

// repositoryMonitorCILogBlobServer serves signed log blobs over TLS and
// records the Authorization header of every request.
type repositoryMonitorCILogBlobServer struct {
	mu      sync.Mutex
	logs    map[string]string
	moved   map[string]string
	auth    []string
	queries []string
	server  *httptest.Server
}

func newRepositoryMonitorCILogBlobServer(t *testing.T) *repositoryMonitorCILogBlobServer {
	t.Helper()
	blobs := &repositoryMonitorCILogBlobServer{logs: map[string]string{}, moved: map[string]string{}}
	blobs.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		blobs.mu.Lock()
		blobs.auth = append(blobs.auth, req.Header.Get("Authorization")+"|"+req.Header.Get("Cookie"))
		blobs.queries = append(blobs.queries, req.URL.RawQuery)
		body, ok := blobs.logs[req.URL.Path]
		target, moved := blobs.moved[req.URL.Path]
		blobs.mu.Unlock()
		switch {
		case moved:
			http.Redirect(w, req, target, http.StatusFound)
		case ok:
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(blobs.server.Close)
	return blobs
}

func (b *repositoryMonitorCILogBlobServer) url(path string) string {
	return b.server.URL + path + "?sv=2025&sig=" + "c2lnbmF0dXJlLXZhbHVlLWZvci10ZXN0cw%3D%3D"
}

// client trusts the test server but keeps the production client's policy
// of no redirects, no proxy, and no connection reuse.
func (b *repositoryMonitorCILogBlobServer) client() *http.Client {
	tlsConfig := b.server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	return newRepositoryMonitorCILogBlobClient(func(netip.Addr) bool { return true }, tlsConfig)
}

func repositoryMonitorCILogTestJob(id int64, head string) map[string]any {
	return map[string]any{"id": id, "head_sha": head, "steps": []map[string]any{
		{"name": "Set up job", "number": 1, "conclusion": "success"},
		{"name": "Run tests", "number": 4, "conclusion": "failure"},
	}}
}

func repositoryMonitorCILogTestEvidence(t *testing.T, github *repositoryMonitorRepairLabelsGitHub, blobs *repositoryMonitorCILogBlobServer, logs bool) repositoryMonitorCIEvidence {
	t.Helper()
	reconciler, monitor := repositoryMonitorEvidenceTestReconciler(t, github, "ci-log-evidence")
	monitor.Spec.Repair.CIEvidence.Logs = logs
	if blobs != nil {
		reconciler.ciLogBlobClient = blobs.client()
	}
	input := reconciler.buildRepositoryMonitorRepairCIInput(context.Background(), monitor, "orka-agents/orka", repositoryMonitorPullRequest{Number: 51, HeadSHA: "head51"})
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "sig=") || (blobs != nil && strings.Contains(string(encoded), blobs.server.URL)) {
		t.Fatalf("evidence carries a signed log URL: %s", encoded)
	}
	return input.Evidence
}

//nolint:gocyclo // One fixture covers each log outcome the evidence distinguishes.
func TestRepositoryMonitorCILogEvidenceFollowsOneRedirectWithoutCredentials(t *testing.T) {
	blobs := newRepositoryMonitorCILogBlobServer(t)
	blobs.logs["/logs/401"] = "2026-11-16T10:00:00Z ##[group]Run go test ./...\n2026-11-16T10:00:01Z --- FAIL: TestA\n2026-11-16T10:00:02Z ##[error]Process completed with exit code 1.\n"
	blobs.moved["/logs/402"] = blobs.url("/logs/elsewhere")
	blobs.logs["/logs/elsewhere"] = "##[error]should never be read\n"
	blobs.logs["/logs/406"] = strings.Repeat("2026-11-16T10:00:00Z noisy output line\n", (repositoryMonitorCILogReadLimit/40)+1000)
	external := repositoryMonitorEvidenceTestRun(408, "external", "completed", "failure", 0)
	external["app"] = map[string]any{"id": 1, "slug": "circleci-checks"}
	runs := []map[string]any{
		repositoryMonitorEvidenceTestRun(401, "unit", "completed", "failure", 0),
		repositoryMonitorEvidenceTestRun(402, "moved", "completed", "failure", 0),
		repositoryMonitorEvidenceTestRun(403, "insecure", "completed", "failure", 0),
		repositoryMonitorEvidenceTestRun(406, "big", "completed", "failure", 0),
		repositoryMonitorEvidenceTestRun(407, "fourth", "completed", "failure", 0),
		external,
	}
	github := &repositoryMonitorRepairLabelsGitHub{
		runs: map[string][]map[string]any{"head51": runs},
		jobs: map[int64]map[string]any{401: repositoryMonitorCILogTestJob(401, "head51"), 402: repositoryMonitorCILogTestJob(402, "head51"), 403: repositoryMonitorCILogTestJob(403, "head51"), 406: repositoryMonitorCILogTestJob(406, "head51"), 407: repositoryMonitorCILogTestJob(407, "head51")},
		jobLogLocations: map[int64]string{
			401: blobs.url("/logs/401"),
			402: blobs.url("/logs/402"),
			403: strings.Replace(blobs.url("/logs/401"), "https://", "http://", 1),
			406: blobs.url("/logs/406"),
		},
	}
	evidence := repositoryMonitorCILogTestEvidence(t, github, blobs, true)
	unit := repositoryMonitorEvidenceByName(t, evidence, "unit").Log
	if unit == nil || unit.Availability != repositoryMonitorEvidenceAvailable || unit.ExcerptKind != repositoryMonitorCILogExcerptLastError || unit.FailedStep != "Run tests" ||
		!strings.Contains(unit.Excerpt, "--- FAIL: TestA") || strings.Contains(unit.Excerpt, "2026-11-16T") {
		t.Fatalf("unit log = %+v", unit)
	}
	if moved := repositoryMonitorEvidenceByName(t, evidence, "moved").Log; moved == nil || moved.Availability != repositoryMonitorEvidenceUnavailable || moved.Reason != repositoryMonitorCILogReasonRedirectRefused || moved.Excerpt != "" {
		t.Fatalf("redirected blob log = %+v", moved)
	}
	if insecure := repositoryMonitorEvidenceByName(t, evidence, "insecure").Log; insecure == nil || insecure.Reason != repositoryMonitorCILogReasonBadRedirect {
		t.Fatalf("http redirect log = %+v", insecure)
	}
	// The three-check limit is spent; later Actions checks name it.
	if big := repositoryMonitorEvidenceByName(t, evidence, "big").Log; big == nil || big.Reason != repositoryMonitorCILogReasonCheckLimit {
		t.Fatalf("fourth Actions check log = %+v", big)
	}
	if other := repositoryMonitorEvidenceByName(t, evidence, "external"); other.Log != nil {
		t.Fatalf("non-Actions check has a log: %+v", other.Log)
	}
	for _, auth := range blobs.auth {
		if auth != "|" {
			t.Fatalf("blob request carried credentials: %q", auth)
		}
	}
	if len(blobs.auth) != 2 {
		t.Fatalf("blob requests = %d, want the unit log and the refused redirect only", len(blobs.auth))
	}
	for _, read := range github.jobReads {
		if strings.HasPrefix(read, "406") || strings.HasPrefix(read, "407") {
			t.Fatalf("read a job past the check limit: %v", github.jobReads)
		}
	}
}

func TestRepositoryMonitorCILogEvidenceBoundsOversizedLogs(t *testing.T) {
	blobs := newRepositoryMonitorCILogBlobServer(t)
	var body strings.Builder
	body.WriteString("2026-11-16T10:00:00Z ##[error]first failure\n")
	for body.Len() < repositoryMonitorCILogReadLimit+(1<<20) {
		body.WriteString("2026-11-16T10:00:00Z " + strings.Repeat("noisy output ", 15) + "(done)\n")
	}
	blobs.logs["/logs/501"] = body.String()
	github := &repositoryMonitorRepairLabelsGitHub{
		runs:            map[string][]map[string]any{"head51": {repositoryMonitorEvidenceTestRun(501, "big", "completed", "failure", 0)}},
		jobs:            map[int64]map[string]any{501: repositoryMonitorCILogTestJob(501, "head51")},
		jobLogLocations: map[int64]string{501: blobs.url("/logs/501")},
	}
	log := repositoryMonitorEvidenceByName(t, repositoryMonitorCILogTestEvidence(t, github, blobs, true), "big").Log
	if log == nil || !log.ReadTruncated || log.Availability != repositoryMonitorEvidenceTruncated || log.ExcerptKind != repositoryMonitorCILogExcerptErrorInReadPart ||
		!strings.Contains(log.Excerpt, "first failure") || len(log.Excerpt) > repositoryMonitorCILogExcerptBytes {
		t.Fatalf("oversized log = %+v", log)
	}
}

func TestRepositoryMonitorCILogEvidenceDegradesWithoutActionsAccess(t *testing.T) {
	for _, tc := range []struct {
		name             string
		status           int
		wantAvailability string
		wantReason       string
	}{
		{name: "forbidden", status: http.StatusForbidden, wantAvailability: repositoryMonitorEvidenceUnavailable, wantReason: "github_api_status_403"},
		{name: "not found", status: http.StatusNotFound, wantAvailability: repositoryMonitorEvidenceUnavailable, wantReason: "github_api_status_404"},
		{name: "rate limited", status: repositoryMonitorRepairLabelsRateLimited, wantAvailability: repositoryMonitorEvidenceRateLimited, wantReason: "github_api_status_403"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := &repositoryMonitorRepairLabelsGitHub{
				runs:        map[string][]map[string]any{"head51": {repositoryMonitorEvidenceTestRun(601, "unit", "completed", "failure", 0), repositoryMonitorEvidenceTestRun(602, "lint", "completed", "failure", 0)}},
				jobFailures: map[int64]int{601: tc.status},
				jobs:        map[int64]map[string]any{602: repositoryMonitorCILogTestJob(602, "older")},
			}
			evidence := repositoryMonitorCILogTestEvidence(t, github, nil, true)
			unit := repositoryMonitorEvidenceByName(t, evidence, "unit")
			if unit.Log == nil || unit.Log.Availability != tc.wantAvailability || unit.Log.Reason != tc.wantReason || unit.Availability != repositoryMonitorEvidenceAvailable {
				t.Fatalf("unit = %+v log %+v", unit, unit.Log)
			}
			lint := repositoryMonitorEvidenceByName(t, evidence, "lint").Log
			if tc.wantAvailability == repositoryMonitorEvidenceRateLimited {
				if lint == nil || lint.Reason != repositoryMonitorEvidenceReasonAfterRateLimit {
					t.Fatalf("log after a rate limit = %+v", lint)
				}
				return
			}
			if lint == nil || lint.Availability != repositoryMonitorEvidenceStale || lint.Reason != repositoryMonitorCILogReasonHeadMismatch {
				t.Fatalf("log for a job on another head = %+v", lint)
			}
		})
	}
}

func TestRepositoryMonitorCILogEvidenceIsOptIn(t *testing.T) {
	github := &repositoryMonitorRepairLabelsGitHub{
		runs: map[string][]map[string]any{"head51": {repositoryMonitorEvidenceTestRun(701, "unit", "completed", "failure", 0)}},
		jobs: map[int64]map[string]any{701: repositoryMonitorCILogTestJob(701, "head51")},
	}
	evidence := repositoryMonitorCILogTestEvidence(t, github, nil, false)
	if log := repositoryMonitorEvidenceByName(t, evidence, "unit").Log; log != nil || len(github.jobReads) != 0 {
		t.Fatalf("logs disabled, but log = %+v and job reads = %v", log, github.jobReads)
	}
}

func TestRepositoryMonitorCILogBlobClientRefusesPrivateDestinations(t *testing.T) {
	blobs := newRepositoryMonitorCILogBlobServer(t)
	blobs.logs["/logs/801"] = "##[error]local\n"
	github := &repositoryMonitorRepairLabelsGitHub{
		runs:            map[string][]map[string]any{"head51": {repositoryMonitorEvidenceTestRun(801, "unit", "completed", "failure", 0)}},
		jobs:            map[int64]map[string]any{801: repositoryMonitorCILogTestJob(801, "head51")},
		jobLogLocations: map[int64]string{801: blobs.url("/logs/801")},
	}
	reconciler, monitor := repositoryMonitorEvidenceTestReconciler(t, github, "ci-log-private")
	monitor.Spec.Repair.CIEvidence.Logs = true
	// The production client: the test blob server listens on loopback.
	evidence := reconciler.buildRepositoryMonitorRepairCIInput(context.Background(), monitor, "orka-agents/orka", repositoryMonitorPullRequest{Number: 51, HeadSHA: "head51"}).Evidence
	if log := repositoryMonitorEvidenceByName(t, evidence, "unit").Log; log == nil || log.Reason != repositoryMonitorCILogReasonAddressNotAllowed || len(blobs.auth) != 0 {
		t.Fatalf("loopback blob log = %+v, blob requests %d", log, len(blobs.auth))
	}
}

// TestRepositoryMonitorCILogEvidenceRoundTripsThroughThePrompt covers the
// frozen input of a retried handoff: log excerpts parsed back from the
// repair Task's prompt rebuild the same bytes and digest.
func TestRepositoryMonitorCILogEvidenceRoundTripsThroughThePrompt(t *testing.T) {
	blobs := newRepositoryMonitorCILogBlobServer(t)
	blobs.logs["/logs/901"] = "2026-11-16T10:00:00Z go test ./...\n2026-11-16T10:00:01Z ##[error]Process completed with exit code 1.\n"
	github := &repositoryMonitorRepairLabelsGitHub{
		runs:            map[string][]map[string]any{"head51": {repositoryMonitorEvidenceTestRun(901, "unit", "completed", "failure", 0)}},
		jobs:            map[int64]map[string]any{901: repositoryMonitorCILogTestJob(901, "head51")},
		jobLogLocations: map[int64]string{901: blobs.url("/logs/901")},
	}
	reconciler, monitor := repositoryMonitorEvidenceTestReconciler(t, github, "ci-log-roundtrip")
	monitor.Spec.Repair.CIEvidence.Logs = true
	reconciler.ciLogBlobClient = blobs.client()
	pr := repositoryMonitorPullRequest{Number: 51, HeadSHA: "head51"}
	input := reconciler.buildRepositoryMonitorRepairCIInput(context.Background(), monitor, "orka-agents/orka", pr)
	if input.Evidence.Checks[0].Log == nil || input.Evidence.Checks[0].Log.Excerpt == "" {
		t.Fatalf("no log excerpt: %+v", input.Evidence.Checks[0])
	}
	prompt, err := reconciler.buildRepositoryMonitorRepairPromptWithCI(context.Background(), monitor, repositoryMonitorCommandIntentFixCI, "orka-agents/orka", pr, &store.MonitorItem{}, &input)
	if err != nil {
		t.Fatal(err)
	}
	frozen, ok := repositoryMonitorRepairCIInputFromPrompt(prompt, pr.HeadSHA)
	if !ok || frozen.digest() != input.digest() {
		t.Fatalf("frozen input digest = %s ok=%t, want %s", frozen.digest(), ok, input.digest())
	}
	rebuilt, err := reconciler.buildRepositoryMonitorRepairPromptWithCI(context.Background(), monitor, repositoryMonitorCommandIntentFixCI, "orka-agents/orka", pr, &store.MonitorItem{}, &frozen)
	if err != nil || rebuilt != prompt {
		t.Fatalf("rebuilt prompt differs: err=%v", err)
	}
}

// TestRepositoryMonitorCILogEvidenceReportsEmptyLogs covers logs with no
// line to excerpt: they are unavailable as log_empty, not a withheld
// excerpt.
func TestRepositoryMonitorCILogEvidenceReportsEmptyLogs(t *testing.T) {
	blobs := newRepositoryMonitorCILogBlobServer(t)
	blobs.logs["/logs/1001"] = ""
	blobs.logs["/logs/1002"] = "2026-11-16T10:00:00Z ##[group]\n2026-11-16T10:00:01Z ##[endgroup]\n"
	github := &repositoryMonitorRepairLabelsGitHub{
		runs: map[string][]map[string]any{"head51": {
			repositoryMonitorEvidenceTestRun(1001, "empty", "completed", "failure", 0),
			repositoryMonitorEvidenceTestRun(1002, "markers", "completed", "failure", 0),
		}},
		jobs:            map[int64]map[string]any{1001: repositoryMonitorCILogTestJob(1001, "head51"), 1002: repositoryMonitorCILogTestJob(1002, "head51")},
		jobLogLocations: map[int64]string{1001: blobs.url("/logs/1001"), 1002: blobs.url("/logs/1002")},
	}
	evidence := repositoryMonitorCILogTestEvidence(t, github, blobs, true)
	for _, name := range []string{"empty", "markers"} {
		log := repositoryMonitorEvidenceByName(t, evidence, name).Log
		if log == nil || log.Availability != repositoryMonitorEvidenceUnavailable || log.Reason != repositoryMonitorCILogReasonEmpty || log.Excerpt != "" || log.ExcerptKind != "" || log.FailedStep != "Run tests" {
			t.Fatalf("%s log = %+v", name, log)
		}
	}
}

// The window-edge pass analyzes the text the sanitizer keeps, so a format
// rune such as U+200B cannot hide a credential character or an escaped line
// break at a boundary.
func TestRepositoryMonitorCILogOpenEdgesIgnoreFormatRunes(t *testing.T) {
	const zwsp = "\u200b"
	withheld := func(original []string, openStart, openEnd bool) []string {
		lines := append([]string(nil), original...)
		repositoryMonitorCILogWithholdOpenEdges(original, lines, openStart, openEnd)
		return lines
	}
	cut := []string{"dump ab ab" + repositoryMonitorCILogCutMarker, zwsp + "Q3xR9mP7tK2vL8nW4", "##[error]failed"}
	got := withheld(cut, false, false)
	if got[1] != repositoryMonitorEvidenceRedactedLine || got[2] != cut[2] {
		t.Fatalf("after a cut, a format-prefixed fragment must be withheld and the error kept: %q", got)
	}
	end := []string{"build output (step)", "Q3xR9mP7tK2vL8nW4\\" + zwsp}
	got = withheld(end, false, true)
	if got[1] != repositoryMonitorEvidenceRedactedLine {
		t.Fatalf("an escaped break hidden by a format rune at the open end must be withheld: %q", got)
	}
}

// A credential YAML block scalar whose header the rolling window prunes stays
// withheld, including past blank lines in its body.
func TestRepositoryMonitorCILogWithholdsYAMLBlockPastTheWindow(t *testing.T) {
	const ts = "2026-11-16T10:00:00.1234567Z "
	var log strings.Builder
	log.WriteString(ts + "config:\n" + ts + "  password: |\n")
	fragments := make([]string, 0, 80)
	for i := range 80 {
		fragment := fmt.Sprintf("Zq%02dvT8mK3pR6wN", i)
		fragments = append(fragments, fragment)
		log.WriteString(ts + "    " + fragment + "\n")
		if i%10 == 9 {
			log.WriteString(ts + "\n")
		}
	}
	log.WriteString(ts + "  user: deploy-bot\n" + ts + "##[error]Process completed with exit code 1.\n")
	scanner, truncated, err := readRepositoryMonitorCILog(strings.NewReader(log.String()), repositoryMonitorCILogReadLimit)
	if err != nil || truncated {
		t.Fatalf("readRepositoryMonitorCILog() truncated=%t err=%v", truncated, err)
	}
	excerpt, _ := scanner.excerpt(truncated)
	text := repositoryMonitorCILogExcerptText(excerpt)
	if !strings.Contains(text, "##[error]Process completed") || !strings.Contains(text, "user: deploy-bot") {
		t.Fatalf("excerpt = %q, want the error and the sibling key", text)
	}
	for _, fragment := range fragments {
		if strings.Contains(text, fragment) {
			t.Fatalf("excerpt = %q, leaks block fragment %q", text, fragment)
		}
	}
}

// After a raw-line cut, a standalone backslash line continues a credential
// into the lines after it.
func TestRepositoryMonitorCILogCutThroughBackslashLine(t *testing.T) {
	original := []string{"dump ab ab" + repositoryMonitorCILogCutMarker, "\\", "Q3xR9mP7tK2vL8nW4", "##[error]failed"}
	lines := append([]string(nil), original...)
	repositoryMonitorCILogWithholdOpenEdges(original, lines, false, false)
	if lines[1] != repositoryMonitorEvidenceRedactedLine || lines[2] != repositoryMonitorEvidenceRedactedLine || lines[3] != original[3] {
		t.Fatalf("lines = %q, want the backslash and fragment withheld and the error kept", lines)
	}
}
