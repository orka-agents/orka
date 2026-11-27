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

const requesterStampDomain = "orka.requester-stamp.v1"

const MinRequesterStampKeyBytes = 32

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

func RequesterStampValid(key []byte, task *corev1alpha1.Task) bool {
	if task == nil || task.Spec.RequestedBy == nil ||
		task.Annotations[labels.AnnotationRequestedBySource] != labels.RequestedBySourceAPI {
		return false
	}
	want := RequesterStamp(key, task.UID, task.Spec.RequestedBy.Issuer, task.Spec.RequestedBy.Subject)
	got := task.Annotations[labels.AnnotationRequestedByStamp]
	return want != "" && len(got) == len(want) && hmac.Equal([]byte(got), []byte(want))
}

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
