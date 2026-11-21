package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/orka-agents/orka/internal/store"
)

// The scanner fails closed where its state is uncertain: a quoted credential
// value still open past the tracker's limit, a PEM block a read stops in, and
// the indented body after a line whose credential key a raw-line cut
// dropped. Each case crosses the rolling window, so the line that identifies
// the value is no longer in the excerpt.

// repositoryMonitorCILogUncertainFragments returns n distinct synthetic
// value fragments.
func repositoryMonitorCILogUncertainFragments(n int) []string {
	fragments := make([]string, n)
	for i := range fragments {
		fragments[i] = fmt.Sprintf("Zq%03dvT8mK3pR6wN", i)
	}
	return fragments
}

// repositoryMonitorCILogUncertainLines renders each fragment as a
// two-word value line indented by prefix.
func repositoryMonitorCILogUncertainLines(prefix string, fragments []string) string {
	var text strings.Builder
	for _, fragment := range fragments {
		text.WriteString(prefix + fragment + " meadow\n")
	}
	return text.String()
}

// repositoryMonitorCILogUncertainPEM returns a PEM block's lines with a
// blank line after every tenth body line, and the body's distinct marker.
func repositoryMonitorCILogUncertainPEM(bodyLines int, end bool) []string {
	lines := []string{"-----" + "BEGIN RSA PRIVATE KEY-----"}
	for i, line := range repositoryMonitorEvidenceTestPEMBody(bodyLines) {
		lines = append(lines, line)
		if i%10 == 9 {
			lines = append(lines, "")
		}
	}
	if end {
		lines = append(lines, "-----"+"END RSA PRIVATE KEY-----")
	}
	return lines
}

// repositoryMonitorCILogUncertainBody returns n PEM body lines 32
// characters wide, each with prefix, with a line of only prefix after every
// tenth.
func repositoryMonitorCILogUncertainBody(prefix string, n int) string {
	var body strings.Builder
	for i := range n {
		body.WriteString(prefix + fmt.Sprintf("%02d", i%100) + strings.Repeat("Q3xR9+/w", 3) + "Ab12Cd\n")
		if i%10 == 9 {
			body.WriteString(prefix + "\n")
		}
	}
	return body.String()
}

// repositoryMonitorCILogUncertainCut returns a log line longer than the raw
// line limit whose dropped tail is a credential key opening a block value.
func repositoryMonitorCILogUncertainCut(prefix string) string {
	return repositoryMonitorCILogUncertainCutTail(prefix, "password: |")
}

// repositoryMonitorCILogUncertainCutTail returns a log line longer than the
// raw line limit whose dropped tail is tail.
func repositoryMonitorCILogUncertainCutTail(prefix, tail string) string {
	return prefix + "dump " + strings.Repeat("ab ", repositoryMonitorCILogRawLineLimit/3) + tail
}

func TestRepositoryMonitorCILogFailsClosedWhereTheValueEndIsUnknown(t *testing.T) {
	fragments := repositoryMonitorCILogUncertainFragments(100)
	pemBody := "Q3xR9+/w"
	for _, tc := range []struct {
		name string
		text string
		// notWant must stay out of the excerpt; want must stay in it.
		notWant, want []string
		withheldRest  bool
	}{
		// The quoted value's key line leaves the window long before the
		// tracker's limit; past it, later lines are not trusted to end it.
		{name: "quoted value open past the limit",
			text:    "--- FAIL: TestDeploy (0.01s)\nconfig:\n  password: \"violet\n" + repositoryMonitorCILogUncertainLines("    ", fragments) + "    amber forest\"\n  user: deploy-bot",
			notWant: append([]string{"amber forest", "deploy-bot"}, fragments...), withheldRest: true},
		{name: "quoted value closing just past the limit",
			text:    "config:\n  password: \"violet\n" + repositoryMonitorCILogUncertainLines("    ", fragments[:repositoryMonitorEvidenceQuotedScalarMaxLines+2]) + "    amber forest\"\n  user: deploy-bot",
			notWant: append([]string{"amber forest"}, fragments[:repositoryMonitorEvidenceQuotedScalarMaxLines+2]...), withheldRest: true},
		// A quoted value that closes within the limit ends where it
		// closes, and the lines after it stay visible.
		{name: "quoted value closing within the limit",
			text:    "config:\n  password: \"violet\n" + repositoryMonitorCILogUncertainLines("    ", fragments[:60]) + "    amber forest\"\n  user: deploy-bot",
			notWant: append([]string{"amber forest"}, fragments[:60]...), want: []string{"user: deploy-bot", "##[error]Process completed"}},
		// The log ends inside a PEM block with blank lines whose BEGIN
		// line is outside the window.
		{name: "pem block without an end", text: "--- FAIL: TestKey (0.01s)\n" + strings.Join(repositoryMonitorCILogUncertainPEM(80, false), "\n"),
			notWant: []string{pemBody, "PRIVATE KEY"}, withheldRest: true},
		{name: "pem block with blank lines ending after the window", text: strings.Join(repositoryMonitorCILogUncertainPEM(80, true), "\n") + "\n--- FAIL: TestKey (0.01s)",
			notWant: []string{pemBody, "PRIVATE KEY"}, want: []string{"--- FAIL: TestKey (0.01s)", "##[error]Process completed"}},
		{name: "pem end with another label does not end the block", text: "-----BEGIN RSA PRIVATE KEY-----\n" + strings.Join(repositoryMonitorEvidenceTestPEMBody(5), "\n") +
			"\n-----END CERTIFICATE-----\n" + strings.Join(repositoryMonitorEvidenceTestPEMBody(5), "\n") + "\n-----END RSA PRIVATE KEY-----\n--- FAIL: TestKey (0.01s)",
			notWant: []string{pemBody}, want: []string{"--- FAIL: TestKey (0.01s)"}},
		// A raw-line cut drops a block scalar's key: its whole indented
		// body is withheld through the dedent.
		{name: "cut header with a multi-word body", text: repositoryMonitorCILogUncertainCut("") + "\n  violet meadow\n  copper kettle\n  amber forest\nuser: deploy-bot",
			notWant: []string{"violet meadow", "copper kettle", "amber forest"}, want: []string{"dump ab", "user: deploy-bot", "##[error]Process completed"}},
		{name: "cut header with a body past the window", text: repositoryMonitorCILogUncertainCut("") + "\n" + repositoryMonitorCILogUncertainLines("  ", fragments[:60]) + "\n  copper kettle\nuser: deploy-bot",
			notWant: append([]string{"copper kettle"}, fragments[:60]...), want: []string{"user: deploy-bot", "##[error]Process completed"}},
		// The excerpt drops a long line's tail, but the tracker reads the
		// whole line: a PEM BEGIN marker or an opening quote in the dropped
		// tail is followed as on any line, on the diff side the line is on.
		{name: "cut pem begin without an end", text: "--- FAIL: TestKey (0.01s)\n" + repositoryMonitorCILogUncertainCutTail("", "-----"+"BEGIN RSA PRIVATE KEY-----") + "\n" +
			strings.Join(repositoryMonitorCILogUncertainPEM(80, false)[1:], "\n"),
			notWant: []string{pemBody}, withheldRest: true},
		{name: "cut pem begin with an end past the window", text: repositoryMonitorCILogUncertainCutTail("", "-----"+"BEGIN RSA PRIVATE KEY-----") + "\n" +
			strings.Join(repositoryMonitorCILogUncertainPEM(80, true)[1:], "\n") + "\n--- FAIL: TestKey (0.01s)",
			notWant: []string{pemBody}, want: []string{"--- FAIL: TestKey (0.01s)", "##[error]Process completed"}},
		{name: "cut pem begin with short body lines", text: repositoryMonitorCILogUncertainCutTail("", "-----"+"BEGIN OPENSSH PRIVATE KEY-----") + "\nYWJjZGVmZ2hp\n" +
			strings.ReplaceAll(repositoryMonitorCILogUncertainBody("", 80), "+/w", "+/w "),
			notWant: []string{pemBody, "YWJjZGVmZ2hp"}, withheldRest: true},
		// Ordinary lines inside the body do not end the block.
		{name: "cut pem begin past ordinary lines", text: repositoryMonitorCILogUncertainCutTail("", "-----"+"BEGIN RSA PRIVATE KEY-----") + "\n" + repositoryMonitorCILogUncertainBody("", 5) +
			"--- FAIL: TestKey (0.01s)\n" + repositoryMonitorCILogUncertainBody("", 20),
			notWant: []string{pemBody, "TestKey"}, withheldRest: true},
		{name: "cut removed encrypted pem with diff blank lines", text: repositoryMonitorCILogUncertainCutTail("-", "-----"+"BEGIN RSA PRIVATE KEY-----") +
			"\n-Proc-Type: 4,ENCRYPTED\n-DEK-Info: AES-128-CBC,0A1B2C3D\n-\n" + repositoryMonitorCILogUncertainBody("-", 20),
			notWant: []string{pemBody, "DEK-Info"}, withheldRest: true},
		{name: "cut added encrypted pem", text: repositoryMonitorCILogUncertainCutTail("+", "-----"+"BEGIN RSA PRIVATE KEY-----") +
			"\n+Proc-Type: 4,ENCRYPTED\n+DEK-Info: AES-128-CBC,0A1B2C3D\n+\n" + repositoryMonitorCILogUncertainBody("+", 20),
			notWant: []string{pemBody, "DEK-Info"}, withheldRest: true},
		{name: "cut line on one diff side with a body on the other", text: repositoryMonitorCILogUncertainCutTail("-", "-----"+"BEGIN RSA PRIVATE KEY-----") +
			"\n+Proc-Type: 4,ENCRYPTED\n+DEK-Info: AES-128-CBC,0A1B2C3D\n+\n" + repositoryMonitorCILogUncertainBody("+", 20),
			notWant: []string{pemBody, "DEK-Info"}, withheldRest: true},
		// A removed separator that starts with five dashes is not a PEM
		// marker and leaves the added side's block open.
		{name: "removed five-dash line inside an added pem block", text: "+-----" + "BEGIN RSA PRIVATE KEY-----\n" + repositoryMonitorCILogUncertainBody("+", 5) +
			"----- obsolete\n" + repositoryMonitorCILogUncertainBody("+", 60),
			notWant: []string{pemBody}, withheldRest: true},
		{name: "cut quoted credential with unindented continuation lines", text: repositoryMonitorCILogUncertainCutTail("", "password: \"violet") + "\n" +
			repositoryMonitorCILogUncertainLines("", fragments[:60]) + "copper, kettle; amber forest\"\nuser: deploy-bot",
			notWant: append([]string{"amber forest"}, fragments[:60]...), want: []string{"user: deploy-bot", "##[error]Process completed"}},
		// A key rotated in a diff: the removed END line ends only the
		// removed side's block, and the added body stays withheld.
		{name: "pem key rotated in a diff", text: " -----" + "BEGIN RSA PRIVATE KEY-----\n" + repositoryMonitorCILogUncertainBody("-", 3) + "------" + "END RSA PRIVATE KEY----- \n" + repositoryMonitorCILogUncertainBody("+", 80),
			notWant: []string{pemBody}, withheldRest: true},
		// A body line that starts with a diff marker's character stays
		// inside a block that is not a diff.
		{name: "pem body lines starting with a plus", text: "-----" + "BEGIN RSA PRIVATE KEY-----\n" + repositoryMonitorCILogUncertainBody("+", 80),
			notWant: []string{pemBody}, withheldRest: true},
		{name: "cut diff header with a body past the window", text: repositoryMonitorCILogUncertainCut("+ ") + "\n" + repositoryMonitorCILogUncertainLines("+   ", fragments[:60]) + "+user: deploy-bot",
			notWant: fragments[:60], want: []string{"user: deploy-bot", "##[error]Process completed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scanner, truncated, err := readRepositoryMonitorCILog(strings.NewReader(repositoryMonitorCILogLeakLog(tc.text)), repositoryMonitorCILogReadLimit)
			if err != nil || truncated {
				t.Fatalf("readRepositoryMonitorCILog() truncated=%t err=%v", truncated, err)
			}
			excerpt, _ := scanner.excerpt(truncated)
			text := repositoryMonitorCILogExcerptText(excerpt)
			for _, notWant := range tc.notWant {
				if strings.Contains(text, notWant) {
					t.Fatalf("excerpt leaks %q: %q", notWant, text)
				}
			}
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Fatalf("excerpt = %q, want %q", text, want)
				}
			}
			if excerpt.withheldRest != tc.withheldRest {
				t.Fatalf("withheldRest = %t, want %t (excerpt %q)", excerpt.withheldRest, tc.withheldRest, text)
			}
		})
	}
}

// A read that stops inside a PEM block, at the read limit or on a failed
// read, withholds the rest of the block: the BEGIN line is outside the
// window and blank lines separate the body.
func TestRepositoryMonitorCILogReadEndingInsideAPEMBlock(t *testing.T) {
	const ts = repositoryMonitorCILogLeakTimestamp
	for _, block := range []struct {
		name  string
		lines []string
	}{
		{name: "pem block", lines: repositoryMonitorCILogUncertainPEM(200, true)},
		// The BEGIN marker is in the dropped tail of a line cut at the raw
		// line limit, and the body is wrapped at 32 characters.
		{name: "pem block begun past a cut", lines: append([]string{repositoryMonitorCILogUncertainCutTail("", "-----"+"BEGIN OPENSSH PRIVATE KEY-----")},
			strings.Split(repositoryMonitorCILogUncertainBody("", 200)+"-----"+"END OPENSSH PRIVATE KEY-----", "\n")...)},
	} {
		var log strings.Builder
		log.WriteString(ts + "--- FAIL: TestKey (0.01s)\n")
		for _, line := range block.lines {
			log.WriteString(ts + line + "\n")
		}
		log.WriteString(ts + "##[error]key rejected\n")
		full := log.String()
		// Stop at a line break well inside the block.
		cut := strings.Index(full, "\n"+ts+"9") + 1
		for _, tc := range []struct {
			name string
			read func() (*repositoryMonitorCILogScanner, bool, error)
		}{
			{name: "read limit", read: func() (*repositoryMonitorCILogScanner, bool, error) {
				return readRepositoryMonitorCILog(strings.NewReader(full), int64(cut))
			}},
			{name: "interrupted read", read: func() (*repositoryMonitorCILogScanner, bool, error) {
				scanner, _, err := readRepositoryMonitorCILog(io.MultiReader(strings.NewReader(full[:cut]), iotest.ErrReader(errors.New("connection reset by peer"))), repositoryMonitorCILogReadLimit)
				if err == nil {
					return nil, false, errors.New("the interrupted read reported no error")
				}
				// readRepositoryMonitorCILogBlob keeps a failed read's
				// lines, marked truncated.
				return scanner, true, nil
			}},
		} {
			t.Run(block.name+" "+tc.name, func(t *testing.T) {
				scanner, truncated, err := tc.read()
				if err != nil || !truncated {
					t.Fatalf("read truncated=%t err=%v, want a truncated read", truncated, err)
				}
				excerpt, kind := scanner.excerpt(truncated)
				text := repositoryMonitorCILogExcerptText(excerpt)
				for _, notWant := range []string{"Q3xR9+/w", "PRIVATE KEY"} {
					if strings.Contains(text, notWant) {
						t.Fatalf("excerpt leaks %q: %q", notWant, text)
					}
				}
				if kind != repositoryMonitorCILogExcerptTailOfReadPart || !excerpt.withheldRest {
					t.Fatalf("excerpt kind=%q withheldRest=%t, want %q with the rest withheld (%q)", kind, excerpt.withheldRest, repositoryMonitorCILogExcerptTailOfReadPart, text)
				}
			})
		}
	}
}

// The check-output path shares the multi-line tracker: a quoted value open
// past its limit withholds the rest of the field.
func TestRepositoryMonitorEvidenceTextWithholdsQuotedValuePastTheLimit(t *testing.T) {
	fragments := repositoryMonitorCILogUncertainFragments(80)
	input := "--- FAIL: TestDeploy (0.01s)\npassword: \"violet\n" + repositoryMonitorCILogUncertainLines("  ", fragments) + "  amber forest\"\nuser: deploy-bot"
	text, _ := repositoryMonitorEvidenceText(input, 64<<10, 1000)
	if !strings.Contains(text, "--- FAIL: TestDeploy (0.01s)") {
		t.Fatalf("text = %q, want the failure before the value", text)
	}
	for _, notWant := range append([]string{"amber forest", "deploy-bot"}, fragments...) {
		if strings.Contains(text, notWant) {
			t.Fatalf("text leaks %q: %q", notWant, text)
		}
	}
}

// The assembled repair prompt carries none of the values, and marks an
// excerpt that ends inside one.
func TestRepositoryMonitorCILogRepairPromptFailsClosedWhereTheValueEndIsUnknown(t *testing.T) {
	fragments := repositoryMonitorCILogUncertainFragments(100)
	for _, tc := range []struct {
		name         string
		text         string
		secrets      []string
		withheldRest bool
	}{
		{name: "quoted value open past the limit", text: "config:\n  password: \"violet\n" + repositoryMonitorCILogUncertainLines("    ", fragments) + "    amber forest\"",
			secrets: append([]string{"amber forest"}, fragments...), withheldRest: true},
		{name: "pem block without an end", text: "--- FAIL: TestKey (0.01s)\n" + strings.Join(repositoryMonitorCILogUncertainPEM(80, false), "\n"),
			secrets: []string{"Q3xR9+/w"}, withheldRest: true},
		{name: "cut header with a body past the window", text: repositoryMonitorCILogUncertainCut("") + "\n" + repositoryMonitorCILogUncertainLines("  ", fragments[:60]) + "user: deploy-bot",
			secrets: fragments[:60]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blobs := newRepositoryMonitorCILogBlobServer(t)
			blobs.logs["/logs/971"] = repositoryMonitorCILogLeakLog(tc.text)
			github := &repositoryMonitorRepairLabelsGitHub{
				runs:            map[string][]map[string]any{"head51": {repositoryMonitorEvidenceTestRun(971, "deploy", "completed", "failure", 0)}},
				jobs:            map[int64]map[string]any{971: repositoryMonitorCILogTestJob(971, "head51")},
				jobLogLocations: map[int64]string{971: blobs.url("/logs/971")},
			}
			reconciler, monitor := repositoryMonitorEvidenceTestReconciler(t, github, "ci-log-uncertain")
			monitor.Spec.Repair.CIEvidence.Logs = true
			reconciler.ciLogBlobClient = blobs.client()
			pr := repositoryMonitorPullRequest{Number: 51, HeadSHA: "head51"}
			input := reconciler.buildRepositoryMonitorRepairCIInput(context.Background(), monitor, "orka-agents/orka", pr)
			if len(input.Evidence.Checks) == 0 || input.Evidence.Checks[0].Log == nil {
				t.Fatalf("no log evidence: %+v", input.Evidence.Checks)
			}
			prompt, err := reconciler.buildRepositoryMonitorRepairPromptWithCI(context.Background(), monitor, repositoryMonitorCommandIntentFixCI, "orka-agents/orka", pr, &store.MonitorItem{}, &input)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range tc.secrets {
				if strings.Contains(prompt, secret) {
					t.Fatalf("repair prompt leaked %q from the job log:\n%s", secret, prompt)
				}
			}
			log := input.Evidence.Checks[0].Log
			if marked := log.Availability == repositoryMonitorEvidenceTruncated && log.Reason == repositoryMonitorCILogReasonValueWithheld; marked != tc.withheldRest {
				t.Fatalf("log availability=%q reason=%q, want the withheld mark %t", log.Availability, log.Reason, tc.withheldRest)
			}
		})
	}
}
