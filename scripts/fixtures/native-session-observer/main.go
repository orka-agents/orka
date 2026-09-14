// native-session-observer runs inside the E2E runtime Pod. Its stdin receives
// only the exact pool's authentication Secret through a pipe, never a file or
// command argument. Output contains safe identity fields and a comparison hash.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func main() {
	if err := observe(); err != nil {
		// Neither input nor upstream errors belong in E2E logs.
		fmt.Fprintln(os.Stderr, "native runtime observation failed")
		os.Exit(1)
	}
}

func observe() error {
	if len(os.Args) != 5 {
		return errors.New("expected Session UID, generation, runtime instance and Secret UID")
	}
	generation, err := strconv.ParseUint(os.Args[2], 10, 64)
	if err != nil || generation == 0 {
		return errors.New("invalid Session generation")
	}
	var secret struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
		Data map[string][]byte `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 64<<10)).Decode(&secret); err != nil {
		return err
	}
	if secret.Metadata.UID != os.Args[4] {
		return errors.New("authentication Secret identity changed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const baseURL = "http://127.0.0.1:8080"
	probe, err := harnessv2.NewClient(baseURL)
	if err != nil {
		return err
	}
	caps, err := probe.Capabilities(ctx)
	if err != nil {
		return err
	}
	bearer := strings.TrimSpace(string(secret.Data["controller-token"]))
	client, err := harnessv2.NewClient(baseURL,
		harnessv2.WithControllerBearerToken(bearer),
		harnessv2.WithOperationCapabilitySecret(secret.Data["capability-secret"]),
		harnessv2.WithStatusCapabilityBinding(harnessv2.StatusCapabilityBinding{
			RuntimeProfileDigest: caps.RuntimeProfileDigest,
			RuntimeInstanceID:    harnessv2.RuntimeInstanceID(os.Args[3]),
		}),
	)
	if err != nil {
		return err
	}
	status, err := client.Status(ctx)
	if err != nil {
		return err
	}
	if string(status.Fence.RuntimeInstanceID) != os.Args[3] || len(status.Sessions) != 1 {
		return errors.New("runtime identity or session count changed")
	}
	session := status.Sessions[0]
	if string(session.RuntimeSessionUID) != os.Args[1] || session.Generation != generation ||
		session.ProviderSessionID == "" {
		return errors.New("native session identity unavailable")
	}
	method := "new"
	if session.NativeRestoration != nil {
		method = session.NativeRestoration.Method
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		ProviderSessionID string                      `json:"providerSessionID"`
		Generation        uint64                      `json:"generation"`
		RuntimeInstanceID harnessv2.RuntimeInstanceID `json:"runtimeInstanceID"`
		Method            string                      `json:"method"`
		CredentialDigest  string                      `json:"credentialDigest"`
	}{session.ProviderSessionID, session.Generation, status.Fence.RuntimeInstanceID,
		method, fmt.Sprintf("%x", sha256.Sum256([]byte(bearer)))})
}
