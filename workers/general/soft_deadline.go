/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/workerenv"

	"github.com/orka-agents/orka/workers/common"
)

const (
	// softDeadlineExitCode is what timeout(1) exits with, so the Task still
	// ends Failed after its partial output is delivered.
	softDeadlineExitCode = 124

	minSoftDeadlineMargin = 20 * time.Second
	maxSoftDeadlineMargin = 2 * time.Minute
	// commandWaitDelay bounds how long a stopped command's leftover children
	// may hold its output pipes open.
	commandWaitDelay = 5 * time.Second
)

var errSoftDeadline = errors.New("command stopped at the Task's soft deadline")

// softDeadlineMargin is the time reserved before the Job deadline to stop the
// command and deliver what it produced: a tenth of the timeout, from 20 s to
// 2 min, and never more than half of a very short timeout.
func softDeadlineMargin(timeout time.Duration) time.Duration {
	margin := min(max(timeout/10, minSoftDeadlineMargin), maxSoftDeadlineMargin)
	return min(margin, timeout/2)
}

// taskTimeout reads the Task timeout the job builder passes to container
// workers. Zero, missing, or malformed values mean no soft deadline.
func taskTimeout(getenv func(string) string) time.Duration {
	seconds, err := strconv.ParseInt(strings.TrimSpace(getenv(workerenv.TimeoutSeconds)), 10, 64)
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// startSoftDeadline ends the returned context with errSoftDeadline once the
// command has used the timeout minus its margin, measured from start, or at
// once when setup already used it. Only the worker's own deadline does this;
// SIGTERM cancels ctx as before.
func startSoftDeadline(
	ctx context.Context, start time.Time, timeout time.Duration,
) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithDeadlineCause(ctx, start.Add(timeout-softDeadlineMargin(timeout)), errSoftDeadline)
}

// errOutputHeldOpen reports a command that exited successfully while its
// descendants kept its output pipes open past commandWaitDelay.
func errOutputHeldOpen(err error) bool {
	return errors.Is(err, exec.ErrWaitDelay)
}

func stoppedAtSoftDeadline(ctx, runCtx context.Context) bool {
	return ctx.Err() == nil && errors.Is(context.Cause(runCtx), errSoftDeadline)
}

// deliverAtSoftDeadline uploads the artifacts written so far and posts a
// mechanical account of the stopped run, with no model turn. It runs inside
// the margin on a fresh deadline, but a SIGTERM still stops it: after the Job
// deadline or a cancellation the controller no longer accepts delivery.
func deliverAtSoftDeadline(ctx context.Context, start time.Time, timeout time.Duration, output string) error {
	elapsed := time.Since(start).Round(time.Second)
	fmt.Fprintf(os.Stderr, "soft deadline: stopped the command after %s of the %s timeout\n", elapsed, timeout)
	delivery, upload := softDeadlineDelivery(start, timeout, time.Now())
	deliveryCtx, cancel := context.WithDeadline(ctx, delivery)
	defer cancel()
	uploadCtx, cancelUpload := context.WithDeadline(deliveryCtx, upload)
	defer cancelUpload()

	data := map[string]any{
		"reason":         "soft_deadline",
		"timeoutSeconds": int64(timeout / time.Second),
		"elapsedSeconds": int64(elapsed / time.Second),
		"outputBytes":    len(output),
	}
	artifacts, uploadErr := common.UploadArtifactsReportContext(uploadCtx)
	if uploadErr != nil {
		fmt.Fprintf(os.Stderr, "soft deadline: %v\n", uploadErr)
		data["artifactError"] = uploadErr.Error()
	}
	result, err := common.FormatStructuredResult(&common.StructuredResult{
		Outcome:   common.ResultOutcomeUnknown,
		Summary:   softDeadlineSummary(elapsed, timeout, output),
		Data:      data,
		Artifacts: artifacts,
	})
	if err != nil {
		return err
	}
	if err := common.SubmitResultContext(deliveryCtx, result); err != nil {
		return fmt.Errorf("%w; partial result not delivered: %w", errSoftDeadline, err)
	}
	return fmt.Errorf("%w after %s", errSoftDeadline, elapsed)
}

// softDeadlineDelivery bounds delivery by what is left of the timeout, less a
// small allowance (2s, or half the margin of a very short timeout), however
// late it starts. Artifact uploads get two thirds of that, so the result,
// which says what happened, is still posted.
func softDeadlineDelivery(start time.Time, timeout time.Duration, now time.Time) (delivery, upload time.Time) {
	delivery = start.Add(timeout - min(2*time.Second, softDeadlineMargin(timeout)/2))
	return delivery, now.Add(delivery.Sub(now) * 2 / 3)
}

// softDeadlineSummary keeps the end of the output, which shows where a stuck
// command was when it stopped.
func softDeadlineSummary(elapsed, timeout time.Duration, output string) string {
	header := fmt.Sprintf(
		"The command did not finish before the Task's soft deadline (%s of the %s timeout) "+
			"and was stopped, so its outcome is unknown. Output so far:\n\n",
		elapsed, timeout)
	if limit := common.MaxStructuredSummaryChars - len(header); len(output) > limit {
		// Leave room for the marker, and start the tail on a whole character.
		omitted := len(output) - (limit - 64)
		for omitted < len(output) && !utf8.RuneStart(output[omitted]) {
			omitted++
		}
		output = fmt.Sprintf("[%d earlier bytes omitted]\n", omitted) + output[omitted:]
	}
	return header + output
}
