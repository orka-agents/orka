/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"path"
	"strings"
	"time"
)

func contextWithTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func workspaceKey(ref WorkspaceRef) string {
	if ref.Namespace == "" || ref.ClaimName == "" {
		return ""
	}
	return ref.Namespace + "/" + ref.ClaimName
}

func reuseIndexKey(namespace string, template TemplateRef, reuseKey string) string {
	return namespace + "/" + template.Namespace + "/" + template.Name + "/" + reuseKey
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}

func cleanArtifactPath(artifactPath string) (string, error) {
	artifactPath = strings.TrimSpace(artifactPath)
	if artifactPath == "" {
		return "", fmt.Errorf("artifact path is required")
	}
	artifactPath = path.Clean("/" + artifactPath)
	artifactPath = strings.TrimPrefix(artifactPath, "/")
	if artifactPath == "." || artifactPath == "" {
		return "", fmt.Errorf("artifact path is required")
	}
	return artifactPath, nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func truncateBytes(value string, maxBytes int64) (string, bool) {
	if maxBytes < 0 || int64(len(value)) <= maxBytes {
		return value, false
	}
	return value[:int(maxBytes)], true
}

func releaseMessage(reason, fallback string) string {
	if strings.TrimSpace(reason) == "" {
		return fallback
	}
	return fallback + ": " + strings.TrimSpace(reason)
}
