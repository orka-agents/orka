/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package workspace

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFakeExecutorClaimCreationAndReuse(t *testing.T) {
	fixedNow := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	f := NewFakeExecutor(WithNow(func() time.Time { return fixedNow }))

	labels := map[string]string{"orka.ai/task": "task-1"}
	req := ClaimRequest{
		Namespace: "default",
		TaskName:  "task-1",
		Template:  TemplateRef{Namespace: "templates", Name: "coding-agent"},
		ReuseKey:  "session-1",
		Labels:    labels,
	}

	first, err := f.Claim(context.Background(), req)
	if err != nil {
		t.Fatalf("Claim() first error = %v", err)
	}
	if !first.Created || first.Reused {
		t.Fatalf("first Created/Reused = %v/%v, want true/false", first.Created, first.Reused)
	}
	if first.Phase != PhaseReady {
		t.Fatalf("first phase = %s, want %s", first.Phase, PhaseReady)
	}
	if first.Ref.Namespace != "default" || first.Ref.ClaimName == "" || first.Ref.SandboxName == "" {
		t.Fatalf("unexpected first ref: %#v", first.Ref)
	}
	if !first.ClaimedAt.Equal(fixedNow) {
		t.Fatalf("ClaimedAt = %s, want %s", first.ClaimedAt, fixedNow)
	}

	labels["orka.ai/task"] = "mutated"
	desc, err := f.Describe(context.Background(), DescribeRequest{Ref: first.Ref})
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if got := desc.Labels["orka.ai/task"]; got != "task-1" {
		t.Fatalf("stored label = %q, want task-1", got)
	}

	second, err := f.Claim(context.Background(), req)
	if err != nil {
		t.Fatalf("Claim() second error = %v", err)
	}
	if second.Created || !second.Reused {
		t.Fatalf("second Created/Reused = %v/%v, want false/true", second.Created, second.Reused)
	}
	if second.Ref != first.Ref {
		t.Fatalf("reused ref = %#v, want %#v", second.Ref, first.Ref)
	}

	freshReq := req
	freshReq.ReuseKey = ""
	fresh1, err := f.Claim(context.Background(), freshReq)
	if err != nil {
		t.Fatalf("Claim() fresh1 error = %v", err)
	}
	fresh2, err := f.Claim(context.Background(), freshReq)
	if err != nil {
		t.Fatalf("Claim() fresh2 error = %v", err)
	}
	if fresh1.Ref == fresh2.Ref {
		t.Fatalf("fresh claims reused ref %#v without reuse key", fresh1.Ref)
	}
}

func TestFakeExecutorWaitReadyTimeoutAndMarkReady(t *testing.T) {
	f := NewFakeExecutor(WithAutoReady(false), WithReadyPollInterval(time.Millisecond))
	claim, err := f.Claim(context.Background(), ClaimRequest{
		Namespace: "default",
		Template:  TemplateRef{Name: "coding-agent"},
	})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if claim.Phase != PhasePending {
		t.Fatalf("claim phase = %s, want %s", claim.Phase, PhasePending)
	}

	_, err = f.WaitReady(context.Background(), WaitReadyRequest{Ref: claim.Ref, Timeout: 5 * time.Millisecond})
	if !IsKind(err, ErrorKindTimeout) {
		t.Fatalf("WaitReady() error = %v, want kind %s", err, ErrorKindTimeout)
	}

	if err := f.MarkReady(claim.Ref); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	ready, err := f.WaitReady(context.Background(), WaitReadyRequest{Ref: claim.Ref, Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("WaitReady() after MarkReady error = %v", err)
	}
	if ready.Phase != PhaseReady || ready.ReadyAt.IsZero() {
		t.Fatalf("ready result = %#v, want phase ready with ReadyAt", ready)
	}
}

func TestFakeExecutorExecSuccessFailureAndCancellation(t *testing.T) {
	f := NewFakeExecutor()
	claim, err := f.Claim(context.Background(), ClaimRequest{
		Namespace: "default",
		Template:  TemplateRef{Name: "coding-agent"},
	})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}

	f.EnqueueExecResult(ExecResult{Stdout: "ok\n", ExitCode: 0}, nil)
	success, err := f.Exec(context.Background(), ExecRequest{Ref: claim.Ref, Command: []string{"echo", "ok"}})
	if err != nil {
		t.Fatalf("Exec() success error = %v", err)
	}
	if success.Stdout != "ok\n" || success.ExitCode != 0 || !success.Succeeded() {
		t.Fatalf("success result = %#v", success)
	}
	if strings.Join(success.Command, " ") != "echo ok" {
		t.Fatalf("success command = %v", success.Command)
	}

	f.EnqueueExecResult(ExecResult{Stderr: "boom\n", ExitCode: 2}, nil)
	failed, err := f.Exec(context.Background(), ExecRequest{Ref: claim.Ref, Command: []string{"false"}})
	if !IsKind(err, ErrorKindCommandFailed) {
		t.Fatalf("Exec() failure error = %v, want kind %s", err, ErrorKindCommandFailed)
	}
	if failed == nil || failed.ExitCode != 2 || failed.Stderr != "boom\n" {
		t.Fatalf("failed result = %#v, want exit 2 with stderr", failed)
	}

	f.EnqueueExecResult(ExecResult{Stdout: "abcdef", Stderr: "ghijkl", ExitCode: 0}, nil)
	truncated, err := f.Exec(context.Background(), ExecRequest{Ref: claim.Ref, Command: []string{"long"}, MaxOutputBytes: 3})
	if err != nil {
		t.Fatalf("Exec() truncated error = %v", err)
	}
	if truncated.Stdout != "abc" || truncated.Stderr != "ghi" || !truncated.StdoutTruncated || !truncated.StderrTruncated {
		t.Fatalf("truncated result = %#v", truncated)
	}

	started := make(chan struct{})
	f.SetExecHandler(func(ctx context.Context, _ ExecRequest) (ExecResult, error) {
		close(started)
		<-ctx.Done()
		return ExecResult{}, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, execErr := f.Exec(ctx, ExecRequest{Ref: claim.Ref, Command: []string{"sleep", "forever"}})
		done <- execErr
	}()
	<-started
	cancel()
	select {
	case execErr := <-done:
		if !IsKind(execErr, ErrorKindCanceled) {
			t.Fatalf("Exec() canceled error = %v, want kind %s", execErr, ErrorKindCanceled)
		}
	case <-time.After(time.Second):
		t.Fatal("Exec() did not return after context cancellation")
	}

	f.EnqueueExecDelay(50*time.Millisecond, ExecResult{Stdout: "late", ExitCode: 0}, nil)
	_, err = f.Exec(context.Background(), ExecRequest{Ref: claim.Ref, Command: []string{"slow"}, Timeout: 5 * time.Millisecond})
	if !IsKind(err, ErrorKindTimeout) {
		t.Fatalf("Exec() timeout error = %v, want kind %s", err, ErrorKindTimeout)
	}
}

func TestFakeExecutorArtifactUploadDownload(t *testing.T) {
	f := NewFakeExecutor()
	claim, err := f.Claim(context.Background(), ClaimRequest{
		Namespace: "default",
		Template:  TemplateRef{Name: "coding-agent"},
	})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}

	data := []byte("hello")
	upload, err := f.Upload(context.Background(), UploadRequest{
		Ref: claim.Ref,
		Artifacts: []UploadArtifact{
			{Path: "logs/out.txt", Data: data},
			{Path: "/patches/../patch.diff", Data: []byte("diff")},
		},
	})
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if len(upload.Artifacts) != 2 {
		t.Fatalf("uploaded artifacts = %d, want 2", len(upload.Artifacts))
	}
	if upload.Artifacts[0].Path != "logs/out.txt" || upload.Artifacts[0].Size != int64(len(data)) || !strings.HasPrefix(upload.Artifacts[0].Digest, "sha256:") {
		t.Fatalf("first uploaded artifact = %#v", upload.Artifacts[0])
	}

	data[0] = 'H'
	down, err := f.Download(context.Background(), DownloadRequest{Ref: claim.Ref, Paths: []string{"logs/out.txt"}})
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if len(down.Artifacts) != 1 || string(down.Artifacts[0].Data) != "hello" {
		t.Fatalf("downloaded artifact = %#v", down.Artifacts)
	}

	all, err := f.Download(context.Background(), DownloadRequest{Ref: claim.Ref})
	if err != nil {
		t.Fatalf("Download() all error = %v", err)
	}
	if len(all.Artifacts) != 2 || all.Artifacts[0].Path != "logs/out.txt" || all.Artifacts[1].Path != "patch.diff" {
		t.Fatalf("all artifact order = %#v", all.Artifacts)
	}

	_, err = f.Download(context.Background(), DownloadRequest{Ref: claim.Ref, Paths: []string{"missing.txt"}})
	if !IsKind(err, ErrorKindNotFound) {
		t.Fatalf("Download() missing error = %v, want kind %s", err, ErrorKindNotFound)
	}
}

func TestFakeExecutorReleaseDeleteRetainBehavior(t *testing.T) {
	f := NewFakeExecutor()
	req := ClaimRequest{
		Namespace: "default",
		Template:  TemplateRef{Name: "coding-agent"},
		ReuseKey:  "session-1",
	}

	claim, err := f.Claim(context.Background(), req)
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	retained, err := f.Release(context.Background(), ReleaseRequest{Ref: claim.Ref, Retain: true, Reason: "debug"})
	if err != nil {
		t.Fatalf("Release(retain) error = %v", err)
	}
	if !retained.Retained || retained.Released || retained.Phase != PhaseRetained {
		t.Fatalf("retained result = %#v", retained)
	}
	desc, err := f.Describe(context.Background(), DescribeRequest{Ref: claim.Ref})
	if err != nil {
		t.Fatalf("Describe(retained) error = %v", err)
	}
	if !desc.Retained || desc.Phase != PhaseRetained || !strings.Contains(desc.Message, "debug") {
		t.Fatalf("retained description = %#v", desc)
	}

	reused, err := f.Claim(context.Background(), req)
	if err != nil {
		t.Fatalf("Claim() retained reuse error = %v", err)
	}
	if !reused.Reused || reused.Ref != claim.Ref || reused.Phase != PhaseReady {
		t.Fatalf("retained reused result = %#v, original ref %#v", reused, claim.Ref)
	}

	released, err := f.Release(context.Background(), ReleaseRequest{Ref: reused.Ref, Reason: "done"})
	if err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if !released.Released || released.Retained || released.Phase != PhaseReleased {
		t.Fatalf("released result = %#v", released)
	}

	newClaim, err := f.Claim(context.Background(), req)
	if err != nil {
		t.Fatalf("Claim() after non-retained release error = %v", err)
	}
	if !newClaim.Created || newClaim.Ref == claim.Ref {
		t.Fatalf("new claim after non-retained release = %#v, old ref %#v", newClaim, claim.Ref)
	}

	deleted, err := f.Delete(context.Background(), DeleteRequest{Ref: newClaim.Ref, Reason: "cleanup"})
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if !deleted.Deleted || deleted.Phase != PhaseDeleted || !strings.Contains(deleted.Message, "cleanup") {
		t.Fatalf("deleted result = %#v", deleted)
	}
	deletedDesc, err := f.Describe(context.Background(), DescribeRequest{Ref: newClaim.Ref})
	if err != nil {
		t.Fatalf("Describe(deleted) error = %v", err)
	}
	if deletedDesc.Phase != PhaseDeleted || deletedDesc.DeletedAt.IsZero() {
		t.Fatalf("deleted description = %#v", deletedDesc)
	}

	_, err = f.Exec(context.Background(), ExecRequest{Ref: newClaim.Ref, Command: []string{"echo", "nope"}})
	if !IsKind(err, ErrorKindNotFound) {
		t.Fatalf("Exec() deleted error = %v, want kind %s", err, ErrorKindNotFound)
	}
	_, err = f.Release(context.Background(), ReleaseRequest{Ref: newClaim.Ref})
	if !IsKind(err, ErrorKindNotFound) {
		t.Fatalf("Release() deleted error = %v, want kind %s", err, ErrorKindNotFound)
	}
}
