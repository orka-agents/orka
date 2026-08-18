package publisher

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/workspacedelta"
)

const testPublicationRef = "refs/heads/orka/test-publication"

type repositoryFixture struct {
	source       Repository
	target       Repository
	seed         string
	baselineOID  string
	baselineRoot string
	delta        workspacedelta.Result
}

func TestPrepareDeterministicTrustedCommitAndDurableBundle(t *testing.T) {
	fixture := newRepositoryFixture(t, true)
	first := newTestPublisher(t)
	second := newTestPublisher(t)
	request := fixture.prepareRequest("publication-deterministic", "prepare-1", RemoteRef{Absent: true})

	one, err := first.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare first: %v", err)
	}
	request.OperationID = "prepare-2"
	two, err := second.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare second: %v", err)
	}
	if one.CommitOID != two.CommitOID || one.TreeOID != two.TreeOID || one.BundleDigest != two.BundleDigest || one.RequestDigest != two.RequestDigest {
		t.Fatalf("deterministic receipts differ:\nfirst=%#v\nsecond=%#v", one, two)
	}
	firstBundle := mustReadFile(t, one.BundlePath)
	secondBundle := mustReadFile(t, two.BundlePath)
	if !bytes.Equal(firstBundle, secondBundle) {
		t.Fatalf("durable bundle bytes differ despite identical immutable input")
	}
	assertPreparedCommit(t, one, fixture.baselineOID, request.CommitMessage, request.CommitTimestamp)

	request.OperationID = "prepare-reconcile"
	repeated, err := first.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare idempotent retry: %v", err)
	}
	if repeated.OperationID != request.OperationID || repeated.CommitOID != one.CommitOID || repeated.BundleDigest != one.BundleDigest {
		t.Fatalf("idempotent retry = %#v, want same artifact with current operation ID", repeated)
	}

	conflict := request
	conflict.CommitMessage = "different immutable request\n"
	if _, err := first.Prepare(context.Background(), conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("Prepare conflicting reuse error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestPrepareAcceptsExactSourceCommit(t *testing.T) {
	fixture := newRepositoryFixture(t, false)
	publisher := newTestPublisher(t)
	request := fixture.prepareRequest("publication-exact-source", "prepare-exact-source", RemoteRef{Absent: true})
	request.SourceRef = fixture.baselineOID
	var commands [][]string
	publisher.commandRecord = func(command []string) { commands = append(commands, append([]string(nil), command...)) }
	prepared, err := publisher.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare exact source commit: %v", err)
	}
	if prepared.SourceRef != fixture.baselineOID || prepared.BaselineOID != fixture.baselineOID {
		t.Fatalf("prepared exact source = %#v", prepared)
	}
	assertPreparedCommit(t, prepared, fixture.baselineOID, request.CommitMessage, request.CommitTimestamp)
	retry := request
	retry.OperationID = "prepare-exact-source-retry"
	reloaded, err := publisher.Prepare(context.Background(), retry)
	if err != nil {
		t.Fatalf("reload exact-source prepared publication: %v", err)
	}
	if reloaded.SourceRef != fixture.baselineOID || reloaded.CommitOID != prepared.CommitOID || reloaded.BundleDigest != prepared.BundleDigest {
		t.Fatalf("reloaded exact-source publication = %#v, want %#v", reloaded, prepared)
	}
	sourceFetches := 0
	shallowObservations := 0
	for _, command := range commands {
		for index, argument := range command {
			if argument != "fetch" || !slices.Contains(command[index+1:], fixture.source.URL) {
				continue
			}
			sourceFetches++
			if slices.Contains(command[index+1:], "--depth=1") {
				shallowObservations++
			}
		}
	}
	if sourceFetches < 2 || shallowObservations < 1 {
		t.Fatalf("exact-source command trace has %d source fetches and %d shallow observations, want both bounded observation and full bundle preparation", sourceFetches, shallowObservations)
	}
	mismatch := fixture.prepareRequest("publication-exact-source-mismatch", "prepare-exact-source-mismatch", RemoteRef{Absent: true})
	mismatch.SourceRef = fixture.baselineOID
	mismatch.BaselineOID = strings.Repeat("a", 40)
	if _, err := newTestPublisher(t).Prepare(context.Background(), mismatch); err == nil {
		t.Fatal("exact source selector differing from baseline was accepted")
	}
}

func TestPreparePrefixesRelativeRootWithoutOverwritingRootCollision(t *testing.T) {
	seed := filepath.Join(t.TempDir(), "seed")
	mustMkdir(t, seed)
	runTestGit(t, seed, nil, "init", "-b", "main")
	mustWriteFile(t, filepath.Join(seed, "main.go"), "package root\n", 0o644)
	mustWriteFile(t, filepath.Join(seed, "delete.txt"), "root delete\n", 0o644)
	mustWriteFile(t, filepath.Join(seed, "services", "app", "main.go"), "package app\n", 0o644)
	mustWriteFile(t, filepath.Join(seed, "services", "app", "keep.txt"), "keep\n", 0o644)
	mustWriteFile(t, filepath.Join(seed, "services", "app", "delete.txt"), "nested delete\n", 0o644)
	if err := os.Symlink("main.go", filepath.Join(seed, "latest")); err != nil {
		t.Fatalf("create root symlink: %v", err)
	}
	if err := os.Symlink("keep.txt", filepath.Join(seed, "services", "app", "latest")); err != nil {
		t.Fatalf("create nested symlink: %v", err)
	}
	runTestGit(t, seed, fixedCommitEnvironment(), "add", "--all")
	runTestGit(t, seed, fixedCommitEnvironment(), "commit", "-m", "baseline")
	baselineOID := strings.TrimSpace(runTestGit(t, seed, nil, "rev-parse", "HEAD"))

	sourcePath := filepath.Join(t.TempDir(), "source.git")
	runTestGit(t, "", nil, "clone", "--bare", "--", seed, sourcePath)
	targetPath := filepath.Join(t.TempDir(), "fork.git")
	runTestGit(t, "", nil, "init", "--bare", "--", targetPath)

	workspaceRoot := filepath.Join(t.TempDir(), "workspace")
	copyTreeWithoutGit(t, filepath.Join(seed, "services", "app"), workspaceRoot)
	if err := os.Remove(filepath.Join(workspaceRoot, "latest")); err != nil {
		t.Fatalf("restore materialized symlink: %v", err)
	}
	if err := os.Symlink("keep.txt", filepath.Join(workspaceRoot, "latest")); err != nil {
		t.Fatalf("restore materialized symlink: %v", err)
	}
	baseline, err := workspacedelta.Capture(workspaceRoot, workspacedelta.Options{})
	if err != nil {
		t.Fatalf("Capture subpath baseline: %v", err)
	}
	mustWriteFile(t, filepath.Join(workspaceRoot, "main.go"), "package app_changed\n", 0o644)
	mustWriteFile(t, filepath.Join(workspaceRoot, "new.txt"), "nested\n", 0o644)
	if err := os.Remove(filepath.Join(workspaceRoot, "delete.txt")); err != nil {
		t.Fatalf("delete nested file: %v", err)
	}
	if err := os.Remove(filepath.Join(workspaceRoot, "latest")); err != nil {
		t.Fatalf("replace nested symlink: %v", err)
	}
	if err := os.Symlink("new.txt", filepath.Join(workspaceRoot, "latest")); err != nil {
		t.Fatalf("recreate nested symlink: %v", err)
	}
	delta, err := workspacedelta.BuildWithLimits(baseline, workspaceRoot, workspacedelta.IntentWrite, workspacedelta.BuildLimits{})
	if err != nil {
		t.Fatalf("Build subpath delta: %v", err)
	}

	p := newTestPublisher(t)
	request := PrepareRequest{
		PublicationID: "publication-subpath", PublicationGeneration: 1, OperationID: "prepare-subpath",
		Source: Repository{Provider: "local", ID: "repository/source", URL: fileURL(sourcePath)}, SourceRef: "refs/heads/main",
		Target: Repository{Provider: "local", ID: "repository/fork", URL: fileURL(targetPath)}, TargetRef: testPublicationRef,
		BranchClaimGeneration: 1, BaselineOID: baselineOID, RemoteBefore: RemoteRef{Absent: true},
		DeltaArtifact: delta.Artifact, DeltaArtifactDigest: delta.ArtifactDigest, RelativeRoot: "services/app",
		CommitMessage:   "Orka publication publication-subpath\n",
		CommitTimestamp: time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC),
	}
	prepared, err := p.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare subpath publication: %v", err)
	}

	inspect := filepath.Join(t.TempDir(), "inspect.git")
	runTestGit(t, "", nil, "init", "--bare", "--", inspect)
	runTestGit(t, "", nil, "--git-dir="+inspect, "fetch", "--", prepared.BundlePath, prepared.BundleRef+":refs/inspect/prepared")
	rootMain := runTestGit(t, "", nil, "--git-dir="+inspect, "show", prepared.CommitOID+":main.go")
	nestedMain := runTestGit(t, "", nil, "--git-dir="+inspect, "show", prepared.CommitOID+":services/app/main.go")
	rootDelete := runTestGit(t, "", nil, "--git-dir="+inspect, "show", prepared.CommitOID+":delete.txt")
	rootLink := runTestGit(t, "", nil, "--git-dir="+inspect, "show", prepared.CommitOID+":latest")
	nestedLink := runTestGit(t, "", nil, "--git-dir="+inspect, "show", prepared.CommitOID+":services/app/latest")
	if rootMain != "package root\n" {
		t.Fatalf("root collision was overwritten: %q", rootMain)
	}
	if nestedMain != "package app_changed\n" {
		t.Fatalf("nested subpath edit = %q, want updated content", nestedMain)
	}
	if rootDelete != "root delete\n" {
		t.Fatalf("root deletion collision was removed: %q", rootDelete)
	}
	if rootLink != "main.go" || nestedLink != "new.txt" {
		t.Fatalf("symlink collision targets = root %q nested %q", rootLink, nestedLink)
	}
	tree := strings.Split(strings.TrimSpace(runTestGit(t, "", nil, "--git-dir="+inspect, "ls-tree", "-r", "--name-only", prepared.CommitOID)), "\n")
	for _, want := range []string{"delete.txt", "latest", "main.go", "services/app/latest", "services/app/main.go", "services/app/new.txt"} {
		if !slices.Contains(tree, want) {
			t.Fatalf("prepared tree missing %q: %#v", want, tree)
		}
	}
	for _, forbidden := range []string{"new.txt", "services/app/delete.txt", "services/app/services/app/main.go"} {
		if slices.Contains(tree, forbidden) {
			t.Fatalf("prepared tree contains incorrectly rooted path %q: %#v", forbidden, tree)
		}
	}
	conflict := request
	conflict.OperationID = "prepare-subpath-conflict"
	conflict.RelativeRoot = "services/other"
	if _, err := p.Prepare(context.Background(), conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("relative-root idempotency conflict error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestSHA256PreparePublishAndVerify(t *testing.T) {
	fixture := newRepositoryFixtureWithFormat(t, false, "sha256")
	publisher := newTestPublisher(t)
	request := fixture.prepareRequest("publication-sha256", "prepare-sha256", RemoteRef{Absent: true})
	request.SourceRef = fixture.baselineOID
	prepared, err := publisher.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare SHA-256: %v", err)
	}
	if len(prepared.CommitOID) != 64 || len(prepared.TreeOID) != 64 {
		t.Fatalf("SHA-256 prepared OIDs = commit %q tree %q", prepared.CommitOID, prepared.TreeOID)
	}
	claim := branchClaim(fixture.target, RemoteRef{Absent: true})
	if _, err := publisher.Publish(context.Background(), publishRequest(prepared, "publish-sha256", claim)); err != nil {
		t.Fatalf("Publish SHA-256: %v", err)
	}
	receipt, err := publisher.Verify(context.Background(), verifyRequest(prepared, "verify-sha256", claim))
	if err != nil || receipt.Outcome != VerifiedExact {
		t.Fatalf("Verify SHA-256 = %#v, %v", receipt, err)
	}
}

func TestPrepareIgnoresHostileConfigAndRejectsChildGitMetadata(t *testing.T) {
	fixture := newRepositoryFixture(t, true)
	markerRoot := t.TempDir()
	globalConfig := filepath.Join(markerRoot, "global.gitconfig")
	hooks := filepath.Join(markerRoot, "hooks")
	mustMkdir(t, hooks)
	writeExecutable(t, filepath.Join(hooks, "post-checkout"), "#!/bin/sh\ntouch "+shellQuote(filepath.Join(markerRoot, "hook-ran"))+"\n")
	filterScript := filepath.Join(markerRoot, "filter")
	writeExecutable(t, filterScript, "#!/bin/sh\ntouch "+shellQuote(filepath.Join(markerRoot, "filter-ran"))+"\ncat\n")
	config := fmt.Sprintf("[core]\n\thooksPath = %s\n[credential]\n\thelper = !touch %s\n[filter \"evil\"]\n\tclean = %s\n\tsmudge = %s\n[url \"file:///definitely-not-the-repository/\"]\n\tinsteadOf = file://\n",
		hooks, filepath.Join(markerRoot, "credential-ran"), filterScript, filterScript)
	mustWriteFile(t, globalConfig, config, 0o600)
	t.Setenv("HOME", markerRoot)
	t.Setenv("XDG_CONFIG_HOME", markerRoot)
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "0")
	t.Setenv("GIT_DIR", filepath.Join(markerRoot, "trap.git"))
	t.Setenv("GIT_WORK_TREE", markerRoot)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", hooks)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("GIT_SSH_COMMAND", "false")
	t.Setenv("GIT_REPLACE_REF_BASE", "refs/replace-hostile/")
	t.Setenv("GIT_ALTERNATE_OBJECT_DIRECTORIES", filepath.Join(markerRoot, "objects"))
	t.Setenv("GIT_TEMPLATE_DIR", filepath.Join(markerRoot, "hostile-template"))

	publisher := newTestPublisher(t)
	request := fixture.prepareRequest("publication-hostile-config", "prepare-hostile", RemoteRef{Absent: true})
	prepared, err := publisher.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare under hostile parent config: %v", err)
	}
	if prepared.CommitOID == "" {
		t.Fatal("Prepare returned empty commit")
	}
	for _, marker := range []string{"hook-ran", "filter-ran", "credential-ran"} {
		if _, err := os.Stat(filepath.Join(markerRoot, marker)); !os.IsNotExist(err) {
			t.Fatalf("hostile Git configuration executed %s", marker)
		}
	}

	unsafeArtifact := addTarEntry(t, fixture.delta.Artifact, "files/.git/config", []byte("[credential]\nhelper = !false\n"))
	unsafe := request
	unsafe.PublicationID = "publication-child-git"
	unsafe.OperationID = "prepare-child-git"
	unsafe.DeltaArtifact = unsafeArtifact
	unsafe.DeltaArtifactDigest = digestBytes(unsafeArtifact)
	if _, err := publisher.Prepare(context.Background(), unsafe); !errors.Is(err, ErrUnsafeDelta) {
		t.Fatalf("Prepare child .git artifact error = %v, want ErrUnsafeDelta", err)
	}
}

func TestPreflightRejectsBranchMovement(t *testing.T) {
	fixture := newRepositoryFixture(t, false)
	pushRef(t, fixture.seed, fixture.target.URL, fixture.baselineOID, testPublicationRef, false)
	second := commitInClone(t, fixture.source.URL, "main", "movement.txt", "moved\n")
	pushRef(t, second.clone, fixture.target.URL, second.oid, testPublicationRef, false)

	publisher := newTestPublisher(t)
	claim := branchClaim(fixture.target, RemoteRef{OID: fixture.baselineOID})
	result, err := publisher.Preflight(context.Background(), PreflightRequest{Target: fixture.target, Claim: claim})
	if !errors.Is(err, ErrBranchMoved) {
		t.Fatalf("Preflight error = %v, want ErrBranchMoved", err)
	}
	if result.Matches || result.Expected.OID != fixture.baselineOID || result.Observed.OID != second.oid {
		t.Fatalf("Preflight result = %#v", result)
	}
}

func TestPublishExactCASForkVerifyAndNoForcePush(t *testing.T) {
	fixture := newRepositoryFixture(t, false)
	publisher := newTestPublisher(t)
	var commandsMu sync.Mutex
	var commands [][]string
	publisher.commandRecord = func(args []string) {
		commandsMu.Lock()
		defer commandsMu.Unlock()
		commands = append(commands, args)
	}
	request := fixture.prepareRequest("publication-exact", "prepare-exact", RemoteRef{Absent: true})
	prepared, err := publisher.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	runTestGit(t, "", nil, "--git-dir="+fileURLPath(t, fixture.target.URL), "config", "receive.denyNonFastForwards", "true")
	claim := branchClaim(fixture.target, RemoteRef{Absent: true})
	publish := publishRequest(prepared, "publish-exact", claim)
	receipt, err := publisher.Publish(context.Background(), publish)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if receipt.Outcome != PublishAcknowledged {
		t.Fatalf("Publish outcome = %q, want %q", receipt.Outcome, PublishAcknowledged)
	}
	if got := remoteOID(t, fixture.target.URL); got != prepared.CommitOID {
		t.Fatalf("remote OID = %s, want %s", got, prepared.CommitOID)
	}
	if fixture.source.ID == fixture.target.ID {
		t.Fatal("fixture did not exercise fork target support")
	}

	commandsMu.Lock()
	captured := append([][]string(nil), commands...)
	commandsMu.Unlock()
	assertNoForcePush(t, captured, prepared.CommitOID, testPublicationRef)

	beforePushes := countPushCommands(captured)
	repeated, err := publisher.Publish(context.Background(), publish)
	if err != nil || repeated != receipt {
		t.Fatalf("idempotent Publish = %#v, %v; want %#v", repeated, err, receipt)
	}
	commandsMu.Lock()
	afterPushes := countPushCommands(commands)
	commandsMu.Unlock()
	if afterPushes != beforePushes {
		t.Fatalf("idempotent Publish issued another push: before=%d after=%d", beforePushes, afterPushes)
	}
	restarted, newErr := New(Options{
		ArtifactRoot: publisher.artifactRoot, TempRoot: filepath.Join(t.TempDir(), "restart-tmp"),
		VerifyAttempts: 1, PublishTimeout: 10 * time.Second,
	})
	if newErr != nil {
		t.Fatalf("New restarted publisher: %v", newErr)
	}
	restartedReceipt, restartErr := restarted.Publish(context.Background(), publish)
	if restartErr != nil || restartedReceipt != receipt {
		t.Fatalf("restarted idempotent Publish = %#v, %v; want %#v", restartedReceipt, restartErr, receipt)
	}

	verify := verifyRequest(prepared, "verify-exact", claim)
	verification, err := publisher.Verify(context.Background(), verify)
	if err != nil {
		t.Fatalf("Verify exact: %v", err)
	}
	if verification.Outcome != VerifiedExact || verification.ObservedRemote.OID != prepared.CommitOID {
		t.Fatalf("Verify exact = %#v", verification)
	}
}

func TestPublishDetectsMovementBetweenPreflightAndCAS(t *testing.T) {
	fixture := newRepositoryFixture(t, false)
	pushRef(t, fixture.seed, fixture.target.URL, fixture.baselineOID, testPublicationRef, false)
	publisher := newTestPublisher(t)
	prepared, err := publisher.Prepare(context.Background(), fixture.prepareRequest("publication-race", "prepare-race", RemoteRef{OID: fixture.baselineOID}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	movement := commitInClone(t, fixture.source.URL, "main", "race.txt", "other writer\n")
	publisher.beforeCAS = func(_ context.Context, _ PublishRequest) error {
		pushRef(t, movement.clone, fixture.target.URL, movement.oid, testPublicationRef, false)
		return nil
	}
	claim := branchClaim(fixture.target, RemoteRef{OID: fixture.baselineOID})
	receipt, err := publisher.Publish(context.Background(), publishRequest(prepared, "publish-race", claim))
	if !errors.Is(err, ErrCASRejected) {
		t.Fatalf("Publish race error = %v, want ErrCASRejected", err)
	}
	if receipt.Outcome != PublishCASRejected || receipt.ObservedRemote.OID != movement.oid {
		t.Fatalf("Publish race receipt = %#v", receipt)
	}
	if got := remoteOID(t, fixture.target.URL); got != movement.oid {
		t.Fatalf("CAS race overwrote remote: got %s want %s", got, movement.oid)
	}
}

func TestExactLeaseRejectsRaceThatRemainsFastForward(t *testing.T) {
	fixture := newRepositoryFixture(t, false)
	oldOID := fixture.baselineOID
	pushRef(t, fixture.seed, fixture.target.URL, oldOID, testPublicationRef, false)

	mustWriteFile(t, filepath.Join(fixture.seed, "upstream.txt"), "upstream movement\n", 0o644)
	runTestGit(t, fixture.seed, fixedCommitEnvironment(), "add", "--all")
	runTestGit(t, fixture.seed, fixedCommitEnvironment(), "commit", "-m", "upstream movement")
	advancedOID := strings.TrimSpace(runTestGit(t, fixture.seed, nil, "rev-parse", "HEAD"))
	pushRef(t, fixture.seed, fixture.source.URL, advancedOID, "refs/heads/main", false)

	advancedWorkspace := filepath.Join(t.TempDir(), "advanced-workspace")
	copyTreeWithoutGit(t, fixture.seed, advancedWorkspace)
	advancedBaseline, err := workspacedelta.Capture(advancedWorkspace, workspacedelta.Options{})
	if err != nil {
		t.Fatalf("Capture advanced baseline: %v", err)
	}
	mustWriteFile(t, filepath.Join(advancedWorkspace, "keep.txt"), "candidate after advanced baseline\n", 0o644)
	advancedDelta, err := workspacedelta.BuildWithLimits(advancedBaseline, advancedWorkspace, workspacedelta.IntentWrite, workspacedelta.BuildLimits{})
	if err != nil {
		t.Fatalf("Build advanced delta: %v", err)
	}
	fixture.baselineOID = advancedOID
	fixture.delta = advancedDelta

	publisher := newTestPublisher(t)
	prepared, err := publisher.Prepare(context.Background(), fixture.prepareRequest("publication-lease-race", "prepare-lease-race", RemoteRef{OID: oldOID}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	publisher.beforeCAS = func(context.Context, PublishRequest) error {
		pushRef(t, fixture.seed, fixture.target.URL, advancedOID, testPublicationRef, false)
		return nil
	}
	claim := branchClaim(fixture.target, RemoteRef{OID: oldOID})
	receipt, err := publisher.Publish(context.Background(), publishRequest(prepared, "publish-lease-race", claim))
	if !errors.Is(err, ErrCASRejected) || receipt.Outcome != PublishCASRejected {
		t.Fatalf("lease race Publish = %#v, %v", receipt, err)
	}
	if got := remoteOID(t, fixture.target.URL); got != advancedOID {
		t.Fatalf("exact lease failed to preserve raced remote: got %s want %s", got, advancedOID)
	}
	if prepared.CommitOID == advancedOID {
		t.Fatal("test candidate unexpectedly equals raced remote")
	}
}

func TestPublishFastForwardsExistingExactOldOID(t *testing.T) {
	fixture := newRepositoryFixture(t, false)
	pushRef(t, fixture.seed, fixture.target.URL, fixture.baselineOID, testPublicationRef, false)
	publisher := newTestPublisher(t)
	var commands [][]string
	publisher.commandRecord = func(args []string) { commands = append(commands, append([]string(nil), args...)) }
	prepared, err := publisher.Prepare(context.Background(), fixture.prepareRequest("publication-existing", "prepare-existing", RemoteRef{OID: fixture.baselineOID}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	claim := branchClaim(fixture.target, RemoteRef{OID: fixture.baselineOID})
	receipt, err := publisher.Publish(context.Background(), publishRequest(prepared, "publish-existing", claim))
	if err != nil || receipt.Outcome != PublishAcknowledged {
		t.Fatalf("Publish existing = %#v, %v", receipt, err)
	}
	if got := remoteOID(t, fixture.target.URL); got != prepared.CommitOID {
		t.Fatalf("remote OID = %s, want %s", got, prepared.CommitOID)
	}
	lease := "--force-with-lease=" + testPublicationRef + ":" + fixture.baselineOID
	found := false
	for _, args := range commands {
		if slices.Contains(args, "push") {
			found = slices.Contains(args, lease)
			for _, arg := range args {
				if arg == "--force" || arg == "-f" || strings.HasPrefix(arg, "+") {
					t.Fatalf("publisher used force-push argument %q in %v", arg, args)
				}
			}
		}
	}
	if !found {
		t.Fatalf("push did not carry exact old-OID lease %q: %v", lease, commands)
	}
}

func TestPublishRejectsNonFastForwardEvenWhenClaimMatches(t *testing.T) {
	fixture := newRepositoryFixture(t, false)
	unrelated := newUnrelatedCommit(t)
	pushRef(t, unrelated.clone, fixture.target.URL, unrelated.oid, testPublicationRef, false)
	publisher := newTestPublisher(t)
	pushes := 0
	publisher.commandRecord = func(args []string) {
		if slices.Contains(args, "push") {
			pushes++
		}
	}
	prepared, err := publisher.Prepare(context.Background(), fixture.prepareRequest("publication-non-ff", "prepare-non-ff", RemoteRef{OID: unrelated.oid}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	claim := branchClaim(fixture.target, RemoteRef{OID: unrelated.oid})
	if _, err := publisher.Publish(context.Background(), publishRequest(prepared, "publish-non-ff", claim)); !errors.Is(err, ErrCASRejected) {
		t.Fatalf("non-fast-forward Publish error = %v, want ErrCASRejected", err)
	}
	if pushes != 0 {
		t.Fatalf("non-fast-forward publication reached push transport %d times", pushes)
	}
	if got := remoteOID(t, fixture.target.URL); got != unrelated.oid {
		t.Fatalf("non-fast-forward publication changed remote to %s, want %s", got, unrelated.oid)
	}
}

func TestVerifySupersededConflictAndIdempotency(t *testing.T) {
	fixture := newRepositoryFixture(t, false)
	publisher := newTestPublisher(t)
	prepared, err := publisher.Prepare(context.Background(), fixture.prepareRequest("publication-verify", "prepare-verify", RemoteRef{Absent: true}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	claim := branchClaim(fixture.target, RemoteRef{Absent: true})
	if _, err := publisher.Publish(context.Background(), publishRequest(prepared, "publish-verify", claim)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	exactRequest := verifyRequest(prepared, "verify-stable", claim)
	exact, err := publisher.Verify(context.Background(), exactRequest)
	if err != nil || exact.Outcome != VerifiedExact {
		t.Fatalf("initial Verify = %#v, %v", exact, err)
	}

	descendant := commitInClone(t, fixture.target.URL, strings.TrimPrefix(testPublicationRef, "refs/heads/"), "descendant.txt", "after Orka\n")
	pushRef(t, descendant.clone, fixture.target.URL, descendant.oid, testPublicationRef, false)
	stable, err := publisher.Verify(context.Background(), exactRequest)
	if err != nil || stable != exact {
		t.Fatalf("same verify operation changed receipt: %#v, %v; want %#v", stable, err, exact)
	}
	newRequest := exactRequest
	newRequest.OperationID = "verify-superseded"
	superseded, err := publisher.Verify(context.Background(), newRequest)
	if err != nil {
		t.Fatalf("Verify superseded: %v", err)
	}
	if superseded.Outcome != DeliveredSuperseded || superseded.ObservedRemote.OID != descendant.oid || superseded.DescendantProofDigest == "" {
		t.Fatalf("Verify superseded = %#v", superseded)
	}

	unrelated := newUnrelatedCommit(t)
	pushRef(t, unrelated.clone, fixture.target.URL, unrelated.oid, testPublicationRef, true)
	conflictRequest := exactRequest
	conflictRequest.OperationID = "verify-conflict"
	conflict, err := publisher.Verify(context.Background(), conflictRequest)
	if err != nil {
		t.Fatalf("Verify conflict: %v", err)
	}
	if conflict.Outcome != DeliveryConflict || conflict.ObservedRemote.OID != unrelated.oid {
		t.Fatalf("Verify conflict = %#v", conflict)
	}
}

func TestAmbiguousPublishReconcilesWithoutRepush(t *testing.T) {
	fixture := newRepositoryFixture(t, false)
	publisher := newTestPublisher(t)
	var pushCount int
	publisher.commandRecord = func(args []string) {
		if slices.Contains(args, "push") {
			pushCount++
		}
	}
	prepared, err := publisher.Prepare(context.Background(), fixture.prepareRequest("publication-ambiguous", "prepare-ambiguous", RemoteRef{Absent: true}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	publisher.afterPush = func(context.Context, PublishRequest) error { return errors.New("injected lost acknowledgement") }
	claim := branchClaim(fixture.target, RemoteRef{Absent: true})
	request := publishRequest(prepared, "publish-ambiguous", claim)
	receipt, err := publisher.Publish(context.Background(), request)
	if !errors.Is(err, ErrPublicationUnknown) || receipt.Outcome != PublishOutcomeUnknown {
		t.Fatalf("ambiguous Publish = %#v, %v", receipt, err)
	}
	if got := remoteOID(t, fixture.target.URL); got != prepared.CommitOID {
		t.Fatalf("ambiguous push did not update remote: got %s want %s", got, prepared.CommitOID)
	}
	publisher.afterPush = nil
	repeated, err := publisher.Publish(context.Background(), request)
	if !errors.Is(err, ErrPublicationUnknown) || repeated != receipt || pushCount != 1 {
		t.Fatalf("ambiguous idempotent retry = %#v, %v, pushes=%d", repeated, err, pushCount)
	}
	verification, err := publisher.Verify(context.Background(), verifyRequest(prepared, "verify-after-ambiguity", claim))
	if err != nil || verification.Outcome != VerifiedExact {
		t.Fatalf("Verify after ambiguity = %#v, %v", verification, err)
	}
}

func TestVerifyClassifiesBoundedObservationFailureUnknown(t *testing.T) {
	fixture := newRepositoryFixture(t, false)
	publisher := newTestPublisher(t)
	prepared, err := publisher.Prepare(context.Background(), fixture.prepareRequest("publication-verify-unknown", "prepare-verify-unknown", RemoteRef{Absent: true}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	claim := branchClaim(fixture.target, RemoteRef{Absent: true})
	if _, err := publisher.Publish(context.Background(), publishRequest(prepared, "publish-verify-unknown", claim)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	publisher.observeFault = func(context.Context, Repository, string) error { return errors.New("injected read outage") }
	request := verifyRequest(prepared, "verify-unknown", claim)
	receipt, err := publisher.Verify(context.Background(), request)
	if !errors.Is(err, ErrVerificationUnknown) || receipt.Outcome != PublicationOutcomeUnknown || receipt.ObservedRemote != (RemoteRef{}) {
		t.Fatalf("unknown Verify = %#v, %v", receipt, err)
	}
	publisher.observeFault = nil
	repeated, err := publisher.Verify(context.Background(), request)
	if !errors.Is(err, ErrVerificationUnknown) || repeated != receipt {
		t.Fatalf("idempotent unknown Verify = %#v, %v; want %#v", repeated, err, receipt)
	}
	request.OperationID = "verify-recovered"
	recovered, err := publisher.Verify(context.Background(), request)
	if err != nil || recovered.Outcome != VerifiedExact {
		t.Fatalf("recovered Verify = %#v, %v", recovered, err)
	}
}

func TestPublishCancellationBoundary(t *testing.T) {
	t.Run("before boundary", func(t *testing.T) {
		fixture := newRepositoryFixture(t, false)
		publisher := newTestPublisher(t)
		prepared, err := publisher.Prepare(context.Background(), fixture.prepareRequest("publication-cancel-before", "prepare-cancel-before", RemoteRef{Absent: true}))
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		claim := branchClaim(fixture.target, RemoteRef{Absent: true})
		if _, err := publisher.Publish(ctx, publishRequest(prepared, "publish-cancel-before", claim)); !errors.Is(err, context.Canceled) {
			t.Fatalf("Publish canceled before boundary error = %v", err)
		}
		if got := remoteOID(t, fixture.target.URL); got != "" {
			t.Fatalf("canceled pre-boundary publish mutated remote to %s", got)
		}
	})

	t.Run("after boundary settles", func(t *testing.T) {
		fixture := newRepositoryFixture(t, false)
		publisher := newTestPublisher(t)
		prepared, err := publisher.Prepare(context.Background(), fixture.prepareRequest("publication-cancel-after", "prepare-cancel-after", RemoteRef{Absent: true}))
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		publisher.beforeCAS = func(context.Context, PublishRequest) error {
			cancel()
			return nil
		}
		claim := branchClaim(fixture.target, RemoteRef{Absent: true})
		receipt, err := publisher.Publish(ctx, publishRequest(prepared, "publish-cancel-after", claim))
		if err != nil || receipt.Outcome != PublishAcknowledged {
			t.Fatalf("post-boundary canceled Publish = %#v, %v", receipt, err)
		}
		if got := remoteOID(t, fixture.target.URL); got != prepared.CommitOID {
			t.Fatalf("post-boundary settlement remote = %s, want %s", got, prepared.CommitOID)
		}
	})
}

func TestPullRequestIntentUsesExactTuple(t *testing.T) {
	fixture := newRepositoryFixture(t, false)
	intent := PullRequestIntent{
		BaseRepository: fixture.source, BaseRef: "refs/heads/main",
		HeadRepository: fixture.target, HeadRef: testPublicationRef,
		PublicationGeneration: 7, ExpectedHeadOID: fixture.baselineOID,
	}
	key, err := intent.Key()
	if err != nil {
		t.Fatalf("PR intent key: %v", err)
	}
	changed := intent
	changed.PublicationGeneration++
	changedKey, err := changed.Key()
	if err != nil {
		t.Fatalf("changed PR intent key: %v", err)
	}
	if key == changedKey {
		t.Fatal("PR tuple key ignored publication generation")
	}
	URLChanged := intent
	URLChanged.BaseRepository.URL = fixture.target.URL
	URLChangedKey, err := URLChanged.Key()
	if err != nil {
		t.Fatalf("URL-changed PR intent key: %v", err)
	}
	if URLChangedKey != key {
		t.Fatal("PR tuple identity used mutable repository URL instead of provider identity")
	}
	reconciler := pullRequestReconcilerFunc(func(context.Context, PullRequestIntent) (PullRequestReceipt, error) {
		return PullRequestReceipt{IntentKey: key, ForgeID: "pr-17", URL: "https://forge.example/pr/17", State: PullRequestOpen, HeadOID: fixture.baselineOID}, nil
	})
	receipt, err := ReconcilePullRequest(context.Background(), intent, reconciler)
	if err != nil || receipt.IntentKey != key {
		t.Fatalf("ReconcilePullRequest = %#v, %v", receipt, err)
	}
	wrong := pullRequestReconcilerFunc(func(context.Context, PullRequestIntent) (PullRequestReceipt, error) {
		return PullRequestReceipt{IntentKey: changedKey, ForgeID: "pr-17", URL: "https://forge.example/pr/17", State: PullRequestOpen, HeadOID: fixture.baselineOID}, nil
	})
	if _, err := ReconcilePullRequest(context.Background(), intent, wrong); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("wrong PR tuple receipt error = %v, want ErrIdempotencyConflict", err)
	}
}

type pullRequestReconcilerFunc func(context.Context, PullRequestIntent) (PullRequestReceipt, error)

func (f pullRequestReconcilerFunc) Reconcile(ctx context.Context, intent PullRequestIntent) (PullRequestReceipt, error) {
	return f(ctx, intent)
}

func newTestPublisher(t *testing.T) *Publisher {
	t.Helper()
	publisher, err := New(Options{
		ArtifactRoot:   filepath.Join(t.TempDir(), "artifacts"),
		TempRoot:       filepath.Join(t.TempDir(), "tmp"),
		VerifyAttempts: 1,
		VerifyBackoff:  time.Millisecond,
		PublishTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("New publisher: %v", err)
	}
	return publisher
}

func newRepositoryFixture(t *testing.T, attributes bool) repositoryFixture {
	t.Helper()
	return newRepositoryFixtureWithFormat(t, attributes, gitObjectFormatSHA1)
}

func newRepositoryFixtureWithFormat(t *testing.T, attributes bool, objectFormat string) repositoryFixture {
	t.Helper()
	seed := filepath.Join(t.TempDir(), "seed")
	mustMkdir(t, seed)
	initArgs := []string{"init", "-b", "main"}
	if objectFormat != gitObjectFormatSHA1 {
		initArgs = append(initArgs, "--object-format="+objectFormat)
	}
	runTestGit(t, seed, nil, initArgs...)
	mustWriteFile(t, filepath.Join(seed, "keep.txt"), "old\n", 0o644)
	mustWriteFile(t, filepath.Join(seed, "delete.txt"), "delete\n", 0o644)
	mustWriteFile(t, filepath.Join(seed, "bin", "run"), "#!/bin/sh\nexit 0\n", 0o755)
	if attributes {
		mustWriteFile(t, filepath.Join(seed, ".gitattributes"), "*.txt filter=evil\n", 0o644)
	}
	runTestGit(t, seed, fixedCommitEnvironment(), "add", "--all")
	runTestGit(t, seed, fixedCommitEnvironment(), "commit", "-m", "baseline")
	baselineOID := strings.TrimSpace(runTestGit(t, seed, nil, "rev-parse", "HEAD"))
	sourcePath := filepath.Join(t.TempDir(), "source.git")
	runTestGit(t, "", nil, "clone", "--bare", "--", seed, sourcePath)
	targetPath := filepath.Join(t.TempDir(), "fork.git")
	targetInitArgs := []string{"init", "--bare"}
	if objectFormat != gitObjectFormatSHA1 {
		targetInitArgs = append(targetInitArgs, "--object-format="+objectFormat)
	}
	targetInitArgs = append(targetInitArgs, "--", targetPath)
	runTestGit(t, "", nil, targetInitArgs...)

	baselineRoot := filepath.Join(t.TempDir(), "workspace")
	copyTreeWithoutGit(t, seed, baselineRoot)
	baseline, err := workspacedelta.Capture(baselineRoot, workspacedelta.Options{})
	if err != nil {
		t.Fatalf("Capture baseline: %v", err)
	}
	mustWriteFile(t, filepath.Join(baselineRoot, "keep.txt"), "changed\n", 0o600)
	if err := os.Remove(filepath.Join(baselineRoot, "delete.txt")); err != nil {
		t.Fatalf("delete workspace file: %v", err)
	}
	mustWriteFile(t, filepath.Join(baselineRoot, "new.txt"), "new\n", 0o644)
	if err := os.Symlink("keep.txt", filepath.Join(baselineRoot, "latest")); err != nil {
		t.Fatalf("create workspace symlink: %v", err)
	}
	delta, err := workspacedelta.BuildWithLimits(baseline, baselineRoot, workspacedelta.IntentWrite, workspacedelta.BuildLimits{})
	if err != nil {
		t.Fatalf("Build delta: %v", err)
	}
	return repositoryFixture{
		source: Repository{Provider: "local", ID: "repository/source", URL: fileURL(sourcePath)},
		target: Repository{Provider: "local", ID: "repository/fork", URL: fileURL(targetPath)},
		seed:   seed, baselineOID: baselineOID, baselineRoot: baselineRoot, delta: delta,
	}
}

func (f repositoryFixture) prepareRequest(publicationID, operationID string, remoteBefore RemoteRef) PrepareRequest {
	return PrepareRequest{
		PublicationID: publicationID, PublicationGeneration: 1, OperationID: operationID,
		Source: f.source, SourceRef: "refs/heads/main", Target: f.target, TargetRef: testPublicationRef,
		BranchClaimGeneration: 1, BaselineOID: f.baselineOID, RemoteBefore: remoteBefore,
		DeltaArtifact: f.delta.Artifact, DeltaArtifactDigest: f.delta.ArtifactDigest,
		CommitMessage:   "Orka publication " + publicationID + "\n",
		CommitTimestamp: time.Date(2026, time.July, 24, 12, 34, 56, 0, time.UTC),
	}
}

func branchClaim(target Repository, baseline RemoteRef) BranchClaim {
	return BranchClaim{RepositoryID: target.ID, Ref: testPublicationRef, OwnerKind: "Task", OwnerUID: "task-uid-0001", Generation: 1, LastVerified: baseline}
}

func publishRequest(prepared PreparedPublication, operationID string, claim BranchClaim) PublishRequest {
	return PublishRequest{
		PublicationID: prepared.PublicationID, PublicationGeneration: prepared.PublicationGeneration,
		OperationID: operationID, Target: prepared.Target, TargetRef: prepared.TargetRef,
		Claim: claim, RemoteBefore: prepared.RemoteBefore, ExpectedCommitOID: prepared.CommitOID, BundleDigest: prepared.BundleDigest,
	}
}

func verifyRequest(prepared PreparedPublication, operationID string, claim BranchClaim) VerifyRequest {
	return VerifyRequest{
		PublicationID: prepared.PublicationID, PublicationGeneration: prepared.PublicationGeneration,
		OperationID: operationID, Target: prepared.Target, TargetRef: prepared.TargetRef,
		Claim: claim, ExpectedCommitOID: prepared.CommitOID, BundleDigest: prepared.BundleDigest,
	}
}

type commitFixture struct {
	clone string
	oid   string
}

func commitInClone(t *testing.T, repositoryURL, branch, name, content string) commitFixture {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "clone")
	runTestGit(t, "", nil, "clone", "--branch", branch, "--single-branch", "--", repositoryURL, clone)
	mustWriteFile(t, filepath.Join(clone, name), content, 0o644)
	runTestGit(t, clone, fixedCommitEnvironment(), "add", "--all")
	runTestGit(t, clone, fixedCommitEnvironment(), "commit", "-m", "fixture change")
	return commitFixture{clone: clone, oid: strings.TrimSpace(runTestGit(t, clone, nil, "rev-parse", "HEAD"))}
}

func newUnrelatedCommit(t *testing.T) commitFixture {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "unrelated")
	mustMkdir(t, clone)
	runTestGit(t, clone, nil, "init", "-b", "main")
	mustWriteFile(t, filepath.Join(clone, "unrelated.txt"), "unrelated\n", 0o644)
	runTestGit(t, clone, fixedCommitEnvironment(), "add", "--all")
	runTestGit(t, clone, fixedCommitEnvironment(), "commit", "-m", "unrelated")
	return commitFixture{clone: clone, oid: strings.TrimSpace(runTestGit(t, clone, nil, "rev-parse", "HEAD"))}
}

func pushRef(t *testing.T, repository, targetURL, oid, ref string, force bool) {
	t.Helper()
	args := []string{"push"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, "--", targetURL, oid+":"+ref)
	runTestGit(t, repository, nil, args...)
}

func remoteOID(t *testing.T, repositoryURL string) string {
	t.Helper()
	output := strings.TrimSpace(runTestGit(t, "", nil, "ls-remote", "--refs", "--", repositoryURL, testPublicationRef))
	if output == "" {
		return ""
	}
	fields := strings.Fields(output)
	if len(fields) != 2 || fields[1] != testPublicationRef {
		t.Fatalf("ls-remote output = %q", output)
	}
	return fields[0]
}

func assertPreparedCommit(t *testing.T, prepared PreparedPublication, parent, message string, timestamp time.Time) {
	t.Helper()
	repository := filepath.Join(t.TempDir(), "inspect.git")
	runTestGit(t, "", nil, "init", "--bare", "--", repository)
	runTestGit(t, "", nil, "--git-dir="+repository, "fetch", "--", prepared.BundlePath, prepared.BundleRef+":refs/inspect/prepared")
	resolved := strings.TrimSpace(runTestGit(t, "", nil, "--git-dir="+repository, "rev-parse", "refs/inspect/prepared^{commit}"))
	if resolved != prepared.CommitOID {
		t.Fatalf("bundle commit = %s, want %s", resolved, prepared.CommitOID)
	}
	format := "%an%x00%ae%x00%cn%x00%ce%x00%at%x00%ct%x00%P%x00%T%x00%B"
	metadata := runTestGit(t, "", nil, "--git-dir="+repository, "show", "-s", "--format="+format, prepared.CommitOID)
	parts := strings.SplitN(metadata, "\x00", 9)
	if len(parts) == 9 {
		parts[8] = strings.TrimSuffix(parts[8], "\n")
	}
	want := []string{CommitAuthorName, CommitAuthorEmail, CommitAuthorName, CommitAuthorEmail,
		fmt.Sprint(timestamp.Unix()), fmt.Sprint(timestamp.Unix()), parent, prepared.TreeOID, message}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("commit metadata = %#v, want %#v", parts, want)
	}
}

func assertNoForcePush(t *testing.T, commands [][]string, expectedOID, ref string) {
	t.Helper()
	foundPush := false
	for _, args := range commands {
		if !slices.Contains(args, "push") {
			continue
		}
		foundPush = true
		for _, arg := range args {
			if arg == "--force" || arg == "-f" || strings.HasPrefix(arg, "+") {
				t.Fatalf("publisher used force-push argument %q in %v", arg, args)
			}
		}
		lease := "--force-with-lease=" + ref + ":"
		if !slices.Contains(args, lease) {
			t.Fatalf("push lacks exact absent-old-OID lease %q: %v", lease, args)
		}
		if !slices.Contains(args, "refs/orka/prepared:"+ref) {
			t.Fatalf("push lacks non-forced exact refspec: %v", args)
		}
		if slices.Contains(args, expectedOID+":"+ref) {
			t.Fatalf("unexpected raw object refspec: %v", args)
		}
	}
	if !foundPush {
		t.Fatal("no publisher push command captured")
	}
}

func countPushCommands(commands [][]string) int {
	count := 0
	for _, args := range commands {
		if slices.Contains(args, "push") {
			count++
		}
	}
	return count
}

func addTarEntry(t *testing.T, artifact []byte, name string, data []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	reader := tar.NewReader(bytes.NewReader(artifact))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read source tar: %v", err)
		}
		payload, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read source tar payload: %v", err)
		}
		copyHeader := *header
		if err := writer.WriteHeader(&copyHeader); err != nil {
			t.Fatalf("copy tar header: %v", err)
		}
		if _, err := writer.Write(payload); err != nil {
			t.Fatalf("copy tar payload: %v", err)
		}
	}
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("write malicious tar header: %v", err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatalf("write malicious tar payload: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close malicious tar: %v", err)
	}
	return output.Bytes()
}

func copyTreeWithoutGit(t *testing.T, source, target string) {
	t.Helper()
	if err := filepath.Walk(source, func(current string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, current)
		if err != nil {
			return err
		}
		if relative == ".git" || strings.HasPrefix(relative, ".git"+string(filepath.Separator)) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		destination := filepath.Join(target, relative)
		if info.IsDir() {
			return os.MkdirAll(destination, info.Mode().Perm())
		}
		data, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, data, info.Mode().Perm())
	}); err != nil {
		t.Fatalf("copy fixture tree: %v", err)
	}
}

func fileURL(path string) string {
	return (&url.URL{Scheme: repositorySchemeFile, Path: filepath.ToSlash(path)}).String()
}

func fileURLPath(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse file URL: %v", err)
	}
	return parsed.Path
}

func fixedCommitEnvironment() map[string]string {
	return map[string]string{
		"GIT_AUTHOR_NAME": "Fixture", "GIT_AUTHOR_EMAIL": "fixture@example.test",
		"GIT_COMMITTER_NAME": "Fixture", "GIT_COMMITTER_EMAIL": "fixture@example.test",
		"GIT_AUTHOR_DATE": "@1700000000 +0000", "GIT_COMMITTER_DATE": "@1700000000 +0000",
	}
}

func runTestGit(t *testing.T, directory string, extra map[string]string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	environment := append([]string(nil), os.Environ()...)
	environment = append(environment, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	for key, value := range extra {
		environment = append(environment, key+"="+value)
	}
	command.Env = environment
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %q: %v\n%s", args, directory, err, output)
	}
	return string(output)
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	mustWriteFile(t, path, content, 0o755)
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func TestPublicationReclaimRejectsGenerationMismatchAndRemovesCacheIdempotently(t *testing.T) {
	fixture := newRepositoryFixture(t, true)
	runtime := newTestPublisher(t)
	request := fixture.prepareRequest("publication-reclaim", "prepare-reclaim", RemoteRef{Absent: true})
	prepared, err := runtime.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	identityPath := filepath.Join(runtime.artifactRoot, request.PublicationID, publicationIdentityName)
	var identity publicationStorageIdentity
	if err := readCanonical(identityPath, &identity); err != nil {
		t.Fatalf("read publication identity: %v", err)
	}
	if identity.PublicationID != request.PublicationID || identity.PublicationGeneration != request.PublicationGeneration {
		t.Fatalf("publication identity = %#v, want exact request identity", identity)
	}

	_, err = runtime.Reclaim(context.Background(), ReclaimRequest{
		PublicationID: request.PublicationID, PublicationGeneration: request.PublicationGeneration + 1,
	})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("Reclaim generation mismatch error = %v, want ErrIdempotencyConflict", err)
	}
	if _, err := os.Stat(prepared.BundlePath); err != nil {
		t.Fatalf("bundle removed after rejected generation mismatch: %v", err)
	}

	result, err := runtime.Reclaim(context.Background(), ReclaimRequest{
		PublicationID: request.PublicationID, PublicationGeneration: request.PublicationGeneration,
	})
	if err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if !result.Reclaimed || result.PublicationID != request.PublicationID || result.PublicationGeneration != request.PublicationGeneration {
		t.Fatalf("Reclaim result = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(runtime.artifactRoot, request.PublicationID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("publication directory after reclaim error = %v, want not exist", err)
	}

	result, err = runtime.Reclaim(context.Background(), ReclaimRequest{
		PublicationID: request.PublicationID, PublicationGeneration: request.PublicationGeneration,
	})
	if err != nil || !result.Reclaimed {
		t.Fatalf("idempotent Reclaim result = %#v, error = %v", result, err)
	}
}

func TestRestorePreparedPersistsPublicationIdentityForFutureReclaim(t *testing.T) {
	fixture := newRepositoryFixture(t, true)
	source := newTestPublisher(t)
	prepared, err := source.Prepare(context.Background(), fixture.prepareRequest(
		"publication-restore-identity", "prepare-restore-identity", RemoteRef{Absent: true},
	))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	bundle := mustReadFile(t, prepared.BundlePath)
	prepared.BundlePath = ""

	restored := newTestPublisher(t)
	if err := restored.RestorePrepared(context.Background(), prepared, bundle); err != nil {
		t.Fatalf("RestorePrepared: %v", err)
	}
	var identity publicationStorageIdentity
	if err := readCanonical(
		filepath.Join(restored.artifactRoot, prepared.PublicationID, publicationIdentityName), &identity,
	); err != nil {
		t.Fatalf("read restored publication identity: %v", err)
	}
	if identity.PublicationID != prepared.PublicationID || identity.PublicationGeneration != prepared.PublicationGeneration {
		t.Fatalf("restored publication identity = %#v, want exact prepared identity", identity)
	}
}

func TestPublicationCacheRecoversPartialLegacyDirectories(t *testing.T) {
	t.Run("prepare upgrades generation one", func(t *testing.T) {
		runtime := newTestPublisher(t)
		publicationID := "publication-legacy-prepare"
		directory := filepath.Join(runtime.artifactRoot, publicationID)
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, bundleFileName), []byte("partial legacy bundle"), 0o600); err != nil {
			t.Fatal(err)
		}

		opened, err := runtime.ensurePublicationDirectory(publicationID, legacyPublicationCacheGeneration)
		if err != nil {
			t.Fatalf("ensurePublicationDirectory: %v", err)
		}
		if opened != directory {
			t.Fatalf("opened directory = %q, want %q", opened, directory)
		}
		identity, err := runtime.readPublicationIdentityFile(directory)
		if err != nil {
			t.Fatalf("read recovered identity: %v", err)
		}
		if identity.PublicationID != publicationID || identity.PublicationGeneration != legacyPublicationCacheGeneration {
			t.Fatalf("recovered identity = %#v", identity)
		}
	})

	t.Run("reclaim quarantines generation one and rejects later generations", func(t *testing.T) {
		runtime := newTestPublisher(t)
		publicationID := "publication-legacy-reclaim"
		directory := filepath.Join(runtime.artifactRoot, publicationID)
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, bundleFileName), []byte("partial legacy bundle"), 0o600); err != nil {
			t.Fatal(err)
		}

		_, err := runtime.Reclaim(context.Background(), ReclaimRequest{
			PublicationID: publicationID, PublicationGeneration: legacyPublicationCacheGeneration + 1,
		})
		if !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("later-generation reclaim error = %v, want ErrIdempotencyConflict", err)
		}
		if _, err := os.Stat(directory); err != nil {
			t.Fatalf("legacy directory removed after rejected generation: %v", err)
		}

		result, err := runtime.Reclaim(context.Background(), ReclaimRequest{
			PublicationID: publicationID, PublicationGeneration: legacyPublicationCacheGeneration,
		})
		if err != nil {
			t.Fatalf("legacy Reclaim: %v", err)
		}
		if !result.Reclaimed {
			t.Fatalf("legacy Reclaim result = %#v", result)
		}
		if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy directory after reclaim error = %v, want not exist", err)
		}
	})
}

func TestLoadPreparedRejectsSymlinkedPublicationDirectory(t *testing.T) {
	runtime := newTestPublisher(t)
	publicationID := "publication-symlink-cache"
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(runtime.artifactRoot, publicationID)); err != nil {
		t.Fatal(err)
	}

	_, err := runtime.loadPrepared(publicationID)
	if !errors.Is(err, ErrPreparedArtifactCorrupt) {
		t.Fatalf("loadPrepared symlink error = %v, want ErrPreparedArtifactCorrupt", err)
	}
	if _, err := os.Stat(filepath.Join(target, publicationIdentityName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink target identity error = %v, want no file", err)
	}
}
