//go:build !windows

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"

	"github.com/orka-agents/orka/internal/cli/client"
	"github.com/orka-agents/orka/internal/codexstate"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type migrationState struct {
	Direction   string `json:"direction"`
	Server      string `json:"server"`
	Namespace   string `json:"namespace"`
	Session     string `json:"session"`
	Home        string `json:"home"`
	Thread      string `json:"thread,omitempty"`
	CWD         string `json:"cwd,omitempty"`
	OperationID string `json:"operationID"`
	Data        []byte `json:"data"`
}

// Automatic migrations use authenticated Kubernetes streams without a local
// listener. The cluster and Service UID, not the transport, identify the target.
func newMigrationClient(cmd *cobra.Command) (*client.Client, string, func(), error) {
	server, _ := cmd.Flags().GetString("server")
	if server == "" {
		server = loadConfig().Server
	}
	if server != "" {
		return newClientFromCmdWithServer(cmd, server), server, func() {}, nil
	}
	kubeconfigPath, _ := cmd.Flags().GetString("kubeconfig")
	restConfig, err := buildRESTConfig(kubeconfigPath)
	if err != nil {
		return nil, "", nil, errors.New("resolve native migration target: invalid or unavailable kubeconfig; use --server for an explicit endpoint")
	}
	// Resolve the same namespace defaults without selecting a transport yet.
	c := newClientFromCmdWithServer(cmd, defaultServer)
	serviceNamespace, serviceName := discoverService(kubeconfigPath, c.Namespace)
	if serviceName == "" {
		return nil, "", nil, errors.New("resolve native migration target: Orka service not found; use --server for an explicit endpoint")
	}
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, "", nil, errors.New("resolve native migration target")
	}
	service, err := kube.CoreV1().Services(serviceNamespace).Get(cmd.Context(), serviceName, metav1.GetOptions{})
	if err != nil || service.UID == "" {
		return nil, "", nil, errors.New("resolve native migration service identity")
	}
	identity, err := json.Marshal(struct {
		Cluster   string
		Namespace string
		Service   string
		UID       string
	}{restConfig.Host, serviceNamespace, serviceName, string(service.UID)})
	if err != nil {
		return nil, "", nil, err
	}
	httpClient, cleanup := newMigrationHTTPClient(cmd.Context(), restConfig, kube, service)
	c.HTTPClient = httpClient
	c.BaseURL = "http://" + migrationTunnelHost
	return c, "kubernetes:" + codexstate.DataDigest(identity), cleanup, nil
}

func migrationDir(name string, create bool) (string, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	if create {
		if err := os.MkdirAll(abs, 0o700); err != nil {
			return "", err
		}
	}
	dir, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if create {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			return "", errors.New("migration destination must be a private directory (0700)")
		}
	}
	return dir, nil
}

func privateMigrationJournal(name, home string) (string, error) {
	dir, err := migrationDir(name, true)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("migration journal must be a private directory (0700)")
	}
	for _, pair := range [][2]string{{home, dir}, {dir, home}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err != nil {
			return "", err
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", errors.New("migration journal and Codex home must not overlap")
		}
	}
	return dir, nil
}

func readMigrationState(journal string, expected migrationState, maxBytes ...int) (*migrationState, error) {
	limit := harnessv2.DefaultMaxNativeSessionBytes
	if len(maxBytes) > 0 {
		var err error
		limit, err = harnessv2.NormalizeNativeSessionMaxBytes(maxBytes[0])
		if err != nil {
			return nil, err
		}
	}
	journalLimit := harnessv2.NativeSessionJSONLimit(limit)
	file, err := os.OpenFile(filepath.Join(journal, "request.json"), os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > int64(journalLimit) {
		return nil, errors.New("invalid migration journal")
	}
	var saved migrationState
	decoder := json.NewDecoder(io.LimitReader(file, int64(journalLimit)+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&saved); err != nil {
		return nil, errors.New("invalid migration journal")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid migration journal tail")
	}
	if !bytes.Equal(identityJSON(saved), identityJSON(expected)) {
		return nil, errors.New("migration journal belongs to another operation or target")
	}
	if saved.OperationID == "" || len(saved.Data) == 0 || len(saved.Data) > limit {
		return nil, errors.New("incomplete migration journal")
	}
	return &saved, nil
}

// Compare only fixed operation identity; Data is deliberately retained from the
// first request so an uncertain retry cannot capture a newer conversation.
func identityJSON(state migrationState) []byte {
	state.Data, state.OperationID = nil, ""
	data, _ := json.Marshal(state)
	return data
}

func persistMigrationState(journal string, state migrationState) error {
	return publishMigrationJSON(journal, "request.json", state)
}

func publishMigrationJSON(journal, target string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(journal, ".request-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() { _ = os.Remove(name) }()
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	// Publish without replacing another invocation's immutable request.
	if err := os.Link(name, filepath.Join(journal, target)); err != nil {
		if errors.Is(err, os.ErrExist) {
			old, readErr := os.ReadFile(filepath.Join(journal, target))
			if readErr == nil && bytes.Equal(old, data) {
				return nil
			}
		}
		return err
	}
	dir, err := os.Open(journal)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func migrationOperationID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "native-" + hex.EncodeToString(random[:]), nil
}

func newSessionMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "migrate", Short: "Move Codex 0.160.0 paginated conversation state"}
	cmd.PersistentFlags().Int("max-bundle-bytes", harnessv2.DefaultMaxNativeSessionBytes, "Maximum encoded native bundle bytes (1..67108864); ORKA_NATIVE_SESSION_MAX_BYTES supplies the default")
	cmd.AddCommand(newSessionMigrateImportCmd(), newSessionMigrateExportCmd())
	return cmd
}

func migrationMaxBundleBytes(cmd *cobra.Command) (int, error) {
	limit := harnessv2.DefaultMaxNativeSessionBytes
	if flag := cmd.Flags().Lookup("max-bundle-bytes"); flag != nil && flag.Changed {
		value, err := cmd.Flags().GetInt("max-bundle-bytes")
		if err != nil {
			return 0, err
		}
		limit = value
	} else if raw, present := os.LookupEnv("ORKA_NATIVE_SESSION_MAX_BYTES"); present && strings.TrimSpace(raw) != "" {
		value, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return 0, errors.New("ORKA_NATIVE_SESSION_MAX_BYTES must be an integer")
		}
		limit = value
	}
	if limit < 1 {
		return 0, errors.New("native bundle limit must be positive")
	}
	return harnessv2.NormalizeNativeSessionMaxBytes(limit)
}

func newSessionMigrateImportCmd() *cobra.Command {
	var home, thread, journal string
	var stopped bool
	cmd := &cobra.Command{
		Use: "import <new-session-name>", Short: "Stage a stopped local Codex thread for its first Orka Task", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			limit, err := migrationMaxBundleBytes(cmd)
			if err != nil {
				return err
			}
			if !stopped {
				return errors.New("stop all source Codex writers, then pass --source-stopped")
			}
			resolvedHome, err := migrationDir(home, false)
			if err != nil {
				return err
			}
			resolvedJournal, err := privateMigrationJournal(journal, resolvedHome)
			if err != nil {
				return err
			}
			c, target, cleanup, err := newMigrationClient(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			c.NativeSessionMaxBytes = limit
			expected := migrationState{Direction: "import", Server: target, Namespace: c.Namespace, Session: args[0], Home: resolvedHome, Thread: thread}
			saved, err := readMigrationState(resolvedJournal, expected, limit)
			if err != nil {
				return err
			}
			if saved == nil {
				expected.Data, err = codexstate.Capture(cmd.Context(), resolvedHome, thread, limit)
				if err != nil {
					return err
				}
				expected.OperationID, err = migrationOperationID()
				if err != nil {
					return err
				}
				if err := persistMigrationState(resolvedJournal, expected); err != nil {
					return fmt.Errorf("persist migration request before import: %w", err)
				}
				saved = &expected
			}
			receipt, err := c.ImportNativeSession(cmd.Context(), args[0], saved.OperationID, saved.Data)
			if err != nil {
				return fmt.Errorf("%w; retry using the same --journal-dir", err)
			}
			if receipt.OperationID != saved.OperationID || receipt.DataDigest != codexstate.DataDigest(saved.Data) || receipt.ProviderSessionID != thread || receipt.SessionName != args[0] || receipt.Namespace != c.Namespace {
				return errors.New("native import receipt does not match the saved request")
			}
			if err := publishMigrationJSON(resolvedJournal, "import-receipt.json", receipt); err != nil {
				return fmt.Errorf("save import receipt: %w; retry using the same --journal-dir", err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Staged thread %s in Session %s. Submit a Task with this Session to continue.\n", thread, args[0])
			return err
		},
	}
	cmd.Flags().StringVar(&home, "codex-home", "", "Source CODEX_HOME (must already exist)")
	cmd.Flags().StringVar(&thread, "thread", "", "Native thread UUID")
	cmd.Flags().StringVar(&journal, "journal-dir", "", "Private directory retained for exact retries")
	cmd.Flags().BoolVar(&stopped, "source-stopped", false, "Confirm every source Codex writer has stopped")
	for _, flag := range []string{"codex-home", "thread", "journal-dir"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	return cmd
}

func newSessionMigrateExportCmd() *cobra.Command {
	var home, cwd, journal, binary string
	cmd := &cobra.Command{
		Use: "export <session-name>", Short: "Install a saved Orka checkpoint into a fresh local Codex home", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			limit, err := migrationMaxBundleBytes(cmd)
			if err != nil {
				return err
			}
			version, err := exec.CommandContext(cmd.Context(), binary, "--version").Output()
			if err != nil || strings.TrimSpace(string(version)) != "codex-cli 0.160.0" {
				return errors.New("destination requires codex-cli 0.160.0 (--codex-bin)")
			}
			resolvedHome, err := migrationDir(home, true)
			if err != nil {
				return err
			}
			resolvedCWD, err := migrationDir(cwd, false)
			if err != nil {
				return err
			}
			resolvedJournal, err := privateMigrationJournal(journal, resolvedHome)
			if err != nil {
				return err
			}
			c, target, cleanup, err := newMigrationClient(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			c.NativeSessionMaxBytes = limit
			expected := migrationState{Direction: "export", Server: target, Namespace: c.Namespace, Session: args[0], Home: resolvedHome, CWD: resolvedCWD}
			saved, err := readMigrationState(resolvedJournal, expected, limit)
			if err != nil {
				return err
			}
			if saved == nil {
				response, err := c.ExportNativeSession(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				summary, err := codexstate.Inspect(cmd.Context(), response.Data, limit)
				if err != nil {
					return err
				}
				if summary.DataDigest != response.DataDigest || summary.ThreadID != response.ProviderSessionID {
					return errors.New("native export identity mismatch")
				}
				expected.Data = response.Data
				expected.OperationID, err = migrationOperationID()
				if err != nil {
					return err
				}
				if err := persistMigrationState(resolvedJournal, expected); err != nil {
					return err
				}
				saved = &expected
			}
			receipt, err := codexstate.Install(cmd.Context(), saved.Data, resolvedHome, resolvedCWD, resolvedJournal, limit)
			if err != nil {
				return fmt.Errorf("install outcome %s: %w; retain the journal and retry it", receipt.Outcome, err)
			}
			summary, err := codexstate.Inspect(cmd.Context(), saved.Data, limit)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Installed thread %s in %s. Authenticate this home with current local credentials, then resume that UUID from %s.\n", summary.ThreadID, resolvedHome, resolvedCWD)
			return err
		},
	}
	cmd.Flags().StringVar(&home, "codex-home", "", "Fresh isolated destination CODEX_HOME")
	cmd.Flags().StringVar(&cwd, "cwd", "", "Existing destination working directory")
	cmd.Flags().StringVar(&journal, "journal-dir", "", "Private directory outside CODEX_HOME, retained for retries")
	cmd.Flags().StringVar(&binary, "codex-bin", "codex", "Codex 0.160.0 executable")
	for _, flag := range []string{"codex-home", "cwd", "journal-dir"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	return cmd
}
