/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package connectors

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

// requesterStampDomain separates the stamp from every other HMAC use of the
// controller key.
const requesterStampDomain = "orka.requester-stamp.v1"

// MinRequesterStampKeyBytes is the smallest stamp key accepted.
const MinRequesterStampKeyBytes = 32

// RequesterStamp computes the stamp that binds a Task's UID to the requester
// the API server verified for it. The UID is server-assigned, so the stamp
// can only be sealed after creation and cannot be transplanted onto another
// Task, even one re-created under the same name.
func RequesterStamp(key []byte, uid types.UID, issuer, subject string) string {
	if len(key) < MinRequesterStampKeyBytes || strings.TrimSpace(string(uid)) == "" {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	for _, part := range []string{requesterStampDomain, string(uid), issuer, subject} {
		mac.Write([]byte(part))
		mac.Write([]byte{0})
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// RequesterStampValid reports whether the Task carries the API server's
// source annotation and a stamp that matches its UID and requester under
// key. Without a key nothing is valid: provenance fails closed.
func RequesterStampValid(key []byte, task *corev1alpha1.Task) bool {
	if task == nil || task.Spec.RequestedBy == nil ||
		task.Annotations[labels.AnnotationRequestedBySource] != labels.RequestedBySourceAPI {
		return false
	}
	want := RequesterStamp(key, task.UID, task.Spec.RequestedBy.Issuer, task.Spec.RequestedBy.Subject)
	got := task.Annotations[labels.AnnotationRequestedByStamp]
	return want != "" && len(got) == len(want) && hmac.Equal([]byte(got), []byte(want))
}

// SealRequesterStamp writes the stamp onto a Task the API server has just
// created and stamped. A Task without the source annotation, or without a
// requester, is left alone. The stamp is patched onto the live object so the
// server-assigned UID is bound.
func SealRequesterStamp(ctx context.Context, c client.Client, key []byte, task *corev1alpha1.Task) error {
	if task == nil || task.Spec.RequestedBy == nil ||
		task.Annotations[labels.AnnotationRequestedBySource] != labels.RequestedBySourceAPI {
		return nil
	}
	if len(key) < MinRequesterStampKeyBytes {
		return errors.New("requester stamp key is not configured")
	}
	stamp := RequesterStamp(key, task.UID, task.Spec.RequestedBy.Issuer, task.Spec.RequestedBy.Subject)
	if stamp == "" {
		return errors.New("created task has no UID to bind the requester stamp to")
	}
	original := task.DeepCopy()
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[labels.AnnotationRequestedByStamp] = stamp
	return c.Patch(ctx, task, client.MergeFrom(original))
}
