/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/transport"

	"github.com/orka-agents/orka/internal/cli/client"
)

const (
	cliProgramName = "orka"
)

const (
	configDir  = ".orka"
	configFile = "config.yaml"

	// defaultNamespace is the Kubernetes default namespace. Service discovery
	// searches it last, and orka dashboard creates its ServiceAccount token there
	// when nothing names a namespace. API requests never fall back to it.
	defaultNamespace = "default"

	// Environment variables that stand in for the global flags of the same name.
	envToken     = "ORKA_TOKEN"
	envServer    = "ORKA_SERVER"
	envNamespace = "ORKA_NAMESPACE"

	// tokenFlagUsage is the help text of --token, including where a command
	// declares its own.
	tokenFlagUsage = "Bearer token for authentication (prefer --token-file or " + envToken +
		", which keep it out of process arguments)"

	// orkaServiceLabel is used to discover the Orka service in a cluster.
	orkaServiceLabel = "app.kubernetes.io/name=orka"
)

// orkaConfig holds the persisted CLI configuration.
type orkaConfig struct {
	Server    string `yaml:"server,omitempty"`
	Token     string `yaml:"token,omitempty"`
	Namespace string `yaml:"namespace,omitempty"`
	// TokenKind is github-session for a session from orka login, which
	// renews itself and is sent only to SessionServer, the server that
	// issued it: changing Server never redirects it.
	TokenKind      string     `yaml:"tokenKind,omitempty"`
	TokenExpiresAt *time.Time `yaml:"tokenExpiresAt,omitempty"`
	SessionServer  string     `yaml:"sessionServer,omitempty"`
	// SessionLogin is the GitHub login the session signed in, for display.
	SessionLogin string `yaml:"sessionLogin,omitempty"`
	// SessionSubject is the numeric GitHub user ID the session signed in.
	// A renewal keeps it, so a successor is told from another person's
	// session: a login can be renamed and reused, the ID cannot.
	SessionSubject string `yaml:"sessionSubject,omitempty"`
}

// portForwardCache holds cached port-forward connection info to avoid re-creating on every command.
type portForwardCache struct {
	Port      int    `json:"port"`
	PID       int    `json:"pid"`
	Service   string `json:"service"`
	Namespace string `json:"namespace"`
	// KubeContext is the kubeTarget cacheKey the forward was started for: the
	// kubeconfig context and the namespaces discovery searched first. The CLI
	// reuses the forward only for that same key.
	KubeContext string `json:"kubeContext,omitempty"`
	// Command is the kubectl command line the CLI started the forward with.
	Command []string `json:"command,omitempty"`
	// ProcessStart is when the forward's process was created, so a later
	// process that reuses its PID is not mistaken for it.
	ProcessStart string `json:"processStart,omitempty"`
	Timestamp    int64  `json:"timestamp"`
}

// portForwardCachePath returns the cache file for the given kubeTarget
// cacheKey, so each context and namespace selection keeps its own forward. It
// returns "" when there is no key or home directory.
func portForwardCachePath(identity string) string {
	if len(identity) < 16 {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, configDir, "portforward-"+identity[:16]+".json")
}

// loadPortForwardCache returns the unexpired forward cached for the given
// kubeTarget cacheKey, or nil.
func loadPortForwardCache(identity string) *portForwardCache {
	path := portForwardCachePath(identity)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cache portForwardCache
	if json.Unmarshal(data, &cache) != nil || cache.KubeContext != identity {
		return nil
	}
	// Expire after 30 minutes
	if time.Now().Unix()-cache.Timestamp > 1800 {
		return nil
	}
	return &cache
}

func savePortForwardCache(cache *portForwardCache) {
	path := portForwardCachePath(cache.KubeContext)
	if path == "" {
		return
	}
	os.MkdirAll(filepath.Dir(path), 0o700) //nolint:errcheck
	cache.Timestamp = time.Now().Unix()
	data, err := json.Marshal(cache)
	if err != nil {
		return
	}
	os.WriteFile(path, data, 0o600) //nolint:errcheck
}

// clearPortForwardCache forgets the forward cached for what cmd would connect
// through now: the current kubeconfig context and the selected namespace.
func clearPortForwardCache(cmd *cobra.Command) {
	if path := portForwardCachePath(commandKubeTarget(cmd, loadConfig()).cacheKey()); path != "" {
		os.Remove(path) //nolint:errcheck
	}
}

// newClientFromCmd creates a client.Client using flag values resolved against
// the environment, config and kubeconfig. It exits on an unreadable token file.
func newClientFromCmd(cmd *cobra.Command) *client.Client {
	c, err := resolveClient(cmd, "")
	cobra.CheckErr(err)
	return c
}

// sessionToken reports whether token is an Orka session, saved by orka login
// or passed with --token, which then goes only to its own server and never
// through a redirect, which can reach another port of the same host with it.
// A session passed with --token crosses the network only as a saved one does:
// over https, or over http only to this machine; otherwise the command stops.
func sessionToken(token string, saved bool, server string) bool {
	if token == "" {
		return false
	}
	if saved {
		return true
	}
	if !strings.HasPrefix(token, sessionTokenPrefix) {
		return false
	}
	cobra.CheckErr(sessionServerAllowed(server))
	return true
}

// newClientWithTunnel is newClientFromCmd for a command that handles
// interrupts itself. A port-forward this call starts normally ends on the
// first interrupt. With ownTunnel the caller decides instead: no interrupt
// handler is registered for the port-forward, the terminal's own interrupt
// does not reach it, and the returned function stops it. That function does
// nothing when no port-forward was started or ownTunnel is false.
func newClientWithTunnel(cmd *cobra.Command, server string, ownTunnel bool) (*client.Client, func()) {
	c, closeTunnel, err := resolveClientWithTunnel(cmd, server, ownTunnel)
	cobra.CheckErr(err)
	return c, closeTunnel
}

// resolveClient picks the server, credentials and namespace for a command.
// A non-empty server is a transport the caller already selected.
//
// The server is --server, ORKA_SERVER or the config file. Without one, the CLI
// reaches Orka through the current kubeconfig context: a cached port-forward
// started for that same context and namespace selection, or a new one to the
// discovered Service. Otherwise it falls back to defaultServer.
//
// The token is --token, --token-file, ORKA_TOKEN or the config file. Only when
// none is set and the server was reached through the kubeconfig does the CLI
// send the kubeconfig user's bearer token, so that credential never goes to a
// server named some other way. The token comes from the same context the
// port-forward goes through, even if the current context changes meanwhile.
//
// The namespace is --namespace, ORKA_NAMESPACE or the config file. Otherwise it
// stays empty and the server uses the namespace it watches. Discovery looks for
// the Service in the selected namespace first, then in the kubeconfig
// context's: the context's namespace belongs to the cluster, not to the Orka
// installation, so requests never carry it.
func resolveClient(cmd *cobra.Command, server string) (*client.Client, error) {
	c, _, err := resolveClientWithTunnel(cmd, server, false)
	return c, err
}

// resolveClientWithTunnel is resolveClient that also returns the function
// stopping a port-forward it started, with ownTunnel as in newClientWithTunnel.
func resolveClientWithTunnel(cmd *cobra.Command, server string, ownTunnel bool) (*client.Client, func(), error) {
	closeTunnel := func() {}
	cfg := loadConfig()
	if server == "" {
		server = configuredServer(cmd, cfg)
	}
	token, saved, err := configuredTokenSource(cmd, cfg)
	if err != nil {
		return nil, closeTunnel, err
	}
	// A session saved by orka login renews itself and goes only to the
	// server that issued it: configuredSession withholds it from any other.
	// A token given with --token, --token-file or ORKA_TOKEN is used as given.
	session := false
	if saved && cfg.TokenKind == tokenKindGitHubSession {
		token, session = configuredSession(cmd, server, cfg), true
	}
	txnToken, err := configuredTxnToken(cmd)
	if err != nil {
		return nil, closeTunnel, err
	}
	ns := configuredNamespace(cmd, cfg)

	if server == "" {
		target := commandKubeTarget(cmd, cfg)
		// Read the context's token, including a tokenFile, before connecting,
		// so a token written to that file meanwhile, perhaps for another
		// context, does not go through this context's port-forward.
		// A session withheld from this server is not replaced by a
		// Kubernetes credential, which would reach that server instead.
		var kubeToken string
		if token == "" && !session {
			kubeToken, _ = target.token()
		}
		server, closeTunnel = connectThroughKubeconfig(target, ownTunnel)
		if server != "" && token == "" && !session {
			token = kubeToken
		}
	}
	if server == "" {
		server = defaultServer
	}

	c := client.NewWithNamespace(server, token, ns)
	c.TxnToken = txnToken
	if sessionToken(token, session, server) {
		c.HTTPClient = noRedirectClient
	}
	return c, closeTunnel, nil
}

// commandKubeTarget is what cmd connects through when no server is named: the
// current context of the --kubeconfig file (or the default kubeconfig), read
// once, and the namespace selected by --namespace, ORKA_NAMESPACE or cfg.
func commandKubeTarget(cmd *cobra.Command, cfg orkaConfig) kubeTarget {
	kubeconfigPath, _ := cmd.Flags().GetString("kubeconfig")
	return kubeTarget{
		kubeconfigContext: currentKubeconfigContext(kubeconfigPath),
		selectedNamespace: configuredNamespace(cmd, cfg),
	}
}

// configuredServer returns the server named by --server, ORKA_SERVER or the
// config file, or "" when none is set.
func configuredServer(cmd *cobra.Command, cfg orkaConfig) string {
	if server, _ := cmd.Flags().GetString("server"); server != "" {
		return server
	}
	if server := strings.TrimSpace(os.Getenv(envServer)); server != "" {
		return server
	}
	return cfg.Server
}

// configuredNamespace returns the namespace named by --namespace,
// ORKA_NAMESPACE or the config file, or "" when none is set.
func configuredNamespace(cmd *cobra.Command, cfg orkaConfig) string {
	if ns, _ := explicitNamespace(cmd); ns != "" {
		return ns
	}
	return cfg.Namespace
}

// explicitNamespace returns the namespace chosen for this command with
// --namespace or ORKA_NAMESPACE, and which of the two chose it, or "" when
// neither is set. A manifest naming another namespace conflicts with it.
func explicitNamespace(cmd *cobra.Command) (string, string) {
	if ns, _ := cmd.Flags().GetString(cliNamespaceQuery); strings.TrimSpace(ns) != "" {
		return strings.TrimSpace(ns), "--namespace"
	}
	if ns := strings.TrimSpace(os.Getenv(envNamespace)); ns != "" {
		return ns, envNamespace
	}
	return "", ""
}

// configuredTokenSource returns the bearer token from --token, --token-file,
// ORKA_TOKEN or the config file, in that order, and whether it is the config
// file's, as a session saved by orka login is.
func configuredTokenSource(cmd *cobra.Command, cfg orkaConfig) (string, bool, error) {
	token, err := flagToken(cmd)
	if err != nil || token != "" {
		return token, false, err
	}
	if token := strings.TrimSpace(os.Getenv(envToken)); token != "" {
		return token, false, nil
	}
	return cfg.Token, cfg.Token != "", nil
}

// flagToken returns the bearer token given by --token or --token-file, or ""
// when neither is set.
func flagToken(cmd *cobra.Command) (string, error) {
	token, _ := cmd.Flags().GetString("token")
	tokenFile, _ := cmd.Flags().GetString("token-file")
	if token != "" && tokenFile != "" {
		return "", errors.New("use either --token or --token-file, not both")
	}
	if token != "" || tokenFile == "" {
		return token, nil
	}
	if txnTokenFile, _ := cmd.Flags().GetString("txn-token-file"); readsStdin(tokenFile) && readsStdin(txnTokenFile) {
		return "", errors.New("--token-file and --txn-token-file cannot both read stdin")
	}
	data, err := readCredentialFile(cmd, "--token-file", tokenFile)
	if err != nil {
		return "", err
	}
	token = strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("--token-file is empty")
	}
	return token, nil
}

// configuredTxnToken returns the transaction token from --txn-token or
// --txn-token-file.
func configuredTxnToken(cmd *cobra.Command) (string, error) {
	txnToken, _ := cmd.Flags().GetString("txn-token")
	txnTokenFile, _ := cmd.Flags().GetString("txn-token-file")
	if txnToken == "" && txnTokenFile != "" {
		data, err := readCredentialFile(cmd, "--txn-token-file", txnTokenFile)
		if err != nil {
			return "", err
		}
		txnToken = string(data)
	}
	return strings.TrimSpace(txnToken), nil
}

// credentialReads remembers each credential file read, so resolving the client
// a second time (orka run retries once) does not find stdin, a pipe or a
// process substitution already drained. An entry is kept only while os.Stdin
// is the file it was read with.
var credentialReads = struct {
	sync.Mutex
	byPath map[string]credentialRead
}{byPath: map[string]credentialRead{}}

type credentialRead struct {
	stdin *os.File
	data  []byte
}

// readsStdin reports whether path names stdin.
func readsStdin(path string) bool {
	switch path {
	case "-", "/dev/stdin", "/dev/fd/0", "/proc/self/fd/0":
		return true
	}
	return false
}

// readCredentialFile reads the credential file named by flag, where "-" is
// stdin. Stdin can feed only one reader, so a command whose --file also reads
// it is rejected.
func readCredentialFile(cmd *cobra.Command, flag, path string) ([]byte, error) {
	if readsStdin(path) {
		if file := cmd.Flags().Lookup("file"); file != nil && readsStdin(file.Value.String()) {
			return nil, fmt.Errorf("%s and --file cannot both read stdin", flag)
		}
	}
	credentialReads.Lock()
	defer credentialReads.Unlock()
	if read, ok := credentialReads.byPath[path]; ok && read.stdin == os.Stdin {
		return read.data, nil
	}
	data, err := readFileOrStdin(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", flag, err)
	}
	credentialReads.byPath[path] = credentialRead{stdin: os.Stdin, data: data}
	return data, nil
}

// rejectStdinCredential returns an error when --token-file or --txn-token-file
// reads stdin, which the command itself uses as use describes.
func rejectStdinCredential(cmd *cobra.Command, use string) error {
	for _, flag := range []string{"token-file", "txn-token-file"} {
		if path, _ := cmd.Flags().GetString(flag); readsStdin(path) {
			return fmt.Errorf("--%s %s reads stdin, which %s", flag, path, use)
		}
	}
	return nil
}

// connectThroughKubeconfig reaches the Orka API through target's kubeconfig
// context, reusing a cached port-forward started for the same target or
// starting a new one to the Service discovered through it. It returns the local
// server URL, or "" when no Service could be reached, and the function that
// stops a new port-forward when ownTunnel is set (see newClientWithTunnel).
func connectThroughKubeconfig(target kubeTarget, ownTunnel bool) (string, func()) {
	closeTunnel := func() {}
	key := target.cacheKey()
	if cached := loadPortForwardCache(key); cached != nil && recordsCurrentForward(cached) && portForwardProcessFn(cached) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", cached.Port), 500*time.Millisecond)
		if err == nil {
			conn.Close() //nolint:errcheck
			fmt.Fprintf(os.Stderr, "Connected to %s in %s (cached port %d)\n",
				cached.Service, cached.Namespace, cached.Port)
			return portForwardURL(cached.Port), closeTunnel
		}
	}

	// Show connecting indicator if stderr is a terminal
	stderrIsTTY := false
	if fi, err := os.Stderr.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		stderrIsTTY = true
	}
	if stderrIsTTY {
		fmt.Fprint(os.Stderr, "⠋ Connecting to cluster…")
	}
	clearIndicator := func() {
		if stderrIsTTY {
			fmt.Fprint(os.Stderr, "\r\033[2K")
		}
	}
	svcNS, svcName := discoverServiceFn(target)
	if svcName == "" {
		clearIndicator()
		return "", closeTunnel
	}
	localPort, pid, cleanup, err := startPortForwardFn(target.kubeconfigContext, svcNS, svcName, ownTunnel)
	if err != nil {
		clearIndicator()
		fmt.Fprintf(os.Stderr, "Could not port-forward to %s in %s: %v\n", svcName, svcNS, err)
		return "", closeTunnel
	}
	// kubectl read the kubeconfig itself before it started forwarding. If the
	// context no longer reaches the cluster the CLI read, or cannot be read,
	// the forward may reach one the CLI's token is not for, so drop it. kubectl
	// was given the namespace, so a change to the context's namespace does not
	// matter.
	if now := readKubeconfigContext(target.path, target.name); now.cluster != target.cluster {
		cleanup()
		clearIndicator()
		fmt.Fprintln(os.Stderr, "The kubeconfig changed while connecting; not using the port-forward")
		return "", closeTunnel
	}
	if ownTunnel {
		closeTunnel = cleanup
	} else {
		// Stop the port-forward on an interrupt. Register before returning, so
		// an interrupt that arrives as soon as the command starts still reaches
		// the handler.
		interrupts := make(chan os.Signal, 1)
		signal.Notify(interrupts, os.Interrupt)
		go func() {
			<-interrupts
			cleanup()
		}()
	}
	savePortForwardCache(&portForwardCache{
		Port:         localPort,
		PID:          pid,
		Service:      svcName,
		Namespace:    svcNS,
		KubeContext:  key,
		Command:      portForwardCommand(target.kubeconfigContext, svcNS, svcName, localPort),
		ProcessStart: processStartMark(pid),
	})
	clearIndicator()
	fmt.Fprintf(os.Stderr, "Connected to %s in %s (port %d)\n",
		svcName, svcNS, localPort)
	return portForwardURL(localPort), closeTunnel
}

// portForwardURL is the API URL of a port-forward on localPort. It names
// 127.0.0.1, the address the CLI checks the forward on, rather than localhost,
// which can resolve to an IPv6 listener the forward does not hold.
func portForwardURL(localPort int) string {
	return fmt.Sprintf("http://127.0.0.1:%d", localPort)
}

// discoverServiceFn, startPortForwardFn, portForwardProcessFn and
// freeLocalPortFn reach the cluster or inspect local processes and ports; tests
// replace them to avoid needing a cluster and kubectl.
var (
	discoverServiceFn    = discoverService
	startPortForwardFn   = startPortForward
	portForwardProcessFn = isPortForwardProcess
	freeLocalPortFn      = freeLocalPort
)

// isPortForwardProcess reports whether the cached forward's process is still
// the kubectl port-forward the CLI started. A cached port is trusted only while
// that process holds it: once the forward exits, any local program could
// listen on the port. Where the process cannot be identified, nothing is
// reused.
func isPortForwardProcess(cached *portForwardCache) bool {
	return cached.PID > 0 && portForwardProcessRunning(cached)
}

// recordsCurrentForward reports whether cached records the port-forward this
// CLI starts for its Service and port: kubectl with the same arguments from
// port-forward on, so listening only on 127.0.0.1. An entry an older CLI wrote
// for another command line is not reused. The kubeconfig file and context the
// command names are already fixed by the cache key, which does not depend on
// how the kubeconfig was named. The arguments are matched from the end because
// a kubeconfig path or context can itself be named port-forward.
func recordsCurrentForward(cached *portForwardCache) bool {
	want := portForwardCommand(kubeconfigContext{}, cached.Namespace, cached.Service, cached.Port)
	at := len(cached.Command) - len(want) + 1
	return at >= 1 && cached.Command[0] == want[0] && slices.Equal(cached.Command[at:], want[1:])
}

// isOwnPortForward reports whether a process owned by uid and running args is
// the forward described by cached, started by this user.
func isOwnPortForward(uid int, args []string, cached *portForwardCache) bool {
	return uid == os.Getuid() && isPortForwardCommand(args, cached.Command)
}

// isPortForwardCommand reports whether args, the command line of a process, is
// want, the kubectl command line the CLI started. Only the base name of the
// program is compared, since a kubectl wrapper can re-execute it by its full
// path. The arguments are compared split on whitespace, as ps reports them.
func isPortForwardCommand(args, want []string) bool {
	if len(args) == 0 || len(want) == 0 || filepath.Base(args[0]) != filepath.Base(want[0]) {
		return false
	}
	return slices.Equal(strings.Fields(strings.Join(args[1:], " ")), strings.Fields(strings.Join(want[1:], " ")))
}

// portForwardCommand is the kubectl command line that forwards localPort to the
// Orka API port of namespace/service through kc. It names kc's context, so
// kubectl uses that context whatever the current one is when it starts. kubectl
// listens only on 127.0.0.1, the address the CLI sends requests to: with its
// default, localhost, it keeps running when it can listen on ::1 but not on
// 127.0.0.1, where another program could then be listening.
func portForwardCommand(kc kubeconfigContext, namespace, service string, localPort int) []string {
	command := []string{"kubectl"}
	if kc.path != "" {
		command = append(command, "--kubeconfig", kc.path)
	}
	if kc.name != "" {
		command = append(command, "--context", kc.name)
	}
	return append(command, "port-forward", "--address", "127.0.0.1", "-n", namespace, "svc/"+service,
		fmt.Sprintf("%d:8080", localPort))
}

// discoverService finds the Orka Service through target's kubeconfig context,
// looking in target's discovery namespaces in order. It returns the namespace
// and Service name, or empty strings if none is found.
func discoverService(target kubeTarget) (string, string) {
	restConfig, err := target.clientConfig().ClientConfig()
	if err != nil {
		return "", ""
	}
	for _, ns := range target.discoveryNamespaces() {
		if name := discoverOrkaService(restConfig, ns); name != "" {
			return ns, name
		}
	}
	return "", ""
}

// portForwardReadyTimeout is how long startPortForward waits for kubectl to
// report that it is forwarding; tests lengthen it.
var portForwardReadyTimeout = 3 * time.Second

// startPortForward starts a kubectl port-forward to the Orka service through
// kc's context. It returns the local port, the process PID, a cleanup function,
// and any error. With isolateInterrupt the port-forward does not receive the
// interrupt a terminal sends to the CLI's foreground process group on Ctrl-C;
// only the cleanup function stops it.
//
// The forward is ready only once kubectl reports that it listens on the port.
// Another local program can take the port after the CLI finds it free and
// before kubectl listens on it. kubectl then cannot listen and exits without
// reporting the port, so the CLI does not mistake that program for the forward.
func startPortForward(kc kubeconfigContext, namespace, service string, isolateInterrupt bool) (int, int, func(), error) {
	localPort, err := freeLocalPortFn()
	if err != nil {
		return 0, 0, nil, fmt.Errorf("find free port: %w", err)
	}

	output, progress, removeOutput, err := portForwardOutput()
	if err != nil {
		return 0, 0, nil, err
	}

	command := portForwardCommand(kc, namespace, service, localPort)
	cmd := exec.Command(command[0], command[1:]...)
	// kubectl's errors go to the same file, so a failure can say why.
	cmd.Stderr = output
	cmd.Stdout = output
	if isolateInterrupt {
		isolateFromTerminalInterrupt(cmd)
	}

	err = cmd.Start()
	output.Close() //nolint:errcheck
	if err != nil {
		progress.Close() //nolint:errcheck
		removeOutput()
		return 0, 0, nil, fmt.Errorf("start port-forward: %w", err)
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait() //nolint:errcheck
		close(exited)
	}()

	cleanup := func() {
		cmd.Process.Kill() //nolint:errcheck
		// Windows removes the output only once kubectl no longer has it open.
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
		}
		removeOutput()
	}

	err = waitForForwarding(progress, localPort, exited, portForwardReadyTimeout)
	progress.Close() //nolint:errcheck
	if err != nil {
		cleanup()
		return 0, 0, nil, err
	}

	return localPort, cmd.Process.Pid, cleanup, nil
}

// freeLocalPort finds a port on 127.0.0.1 that nothing listens on.
func freeLocalPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close() //nolint:errcheck
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// portForwardOutput creates the file kubectl writes its standard output to and
// opens it again for the CLI to read. kubectl outlives the CLI and writes a
// line for every connection, so it cannot write to a pipe, which breaks when the
// CLI exits. Where an open file can be removed, the file is removed at once and
// its space is freed when kubectl exits. Windows cannot remove an open file:
// there remove deletes it after kubectl has stopped, and a forward that
// outlives the CLI leaves it in the temporary directory.
func portForwardOutput() (output, progress *os.File, remove func(), err error) {
	output, err = os.CreateTemp("", "orka-port-forward-*.out")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create port-forward output: %w", err)
	}
	name := output.Name()
	progress, err = os.Open(name)
	if err == nil {
		// Read the file kubectl writes, even if something replaced it by name.
		err = sameFile(output, progress)
		if err != nil {
			progress.Close() //nolint:errcheck
		}
	}
	if err != nil {
		output.Close()  //nolint:errcheck
		os.Remove(name) //nolint:errcheck
		return nil, nil, nil, fmt.Errorf("open port-forward output: %w", err)
	}
	removed := os.Remove(name) == nil
	return output, progress, func() {
		if !removed {
			removed = os.Remove(name) == nil
		}
	}, nil
}

// sameFile returns an error unless a and b are open on the same file.
func sameFile(a, b *os.File) error {
	aInfo, err := a.Stat()
	if err != nil {
		return err
	}
	bInfo, err := b.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(aInfo, bInfo) {
		return fmt.Errorf("%s was replaced", a.Name())
	}
	return nil
}

// waitForForwarding waits for kubectl to write, to the output progress reads,
// that it listens on 127.0.0.1:localPort. It gives up once exited is closed,
// when kubectl has exited, or after timeout, with an error that ends with the
// last line kubectl wrote.
func waitForForwarding(progress io.Reader, localPort int, exited <-chan struct{}, timeout time.Duration) error {
	var seen []byte
	failed := func(reason string) error {
		if last := lastLine(seen); last != "" {
			return fmt.Errorf("%s: %s", reason, last)
		}
		return errors.New(reason)
	}
	deadline := time.After(timeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-exited:
		case <-deadline:
			more, _ := io.ReadAll(progress)
			seen = append(seen, more...)
			return failed(fmt.Sprintf("kubectl port-forward did not report forwarding within %s", timeout))
		case <-tick.C:
		}
		more, _ := io.ReadAll(progress)
		seen = append(seen, more...)
		select {
		case <-exited:
			// Read what kubectl wrote before it exited.
			more, _ := io.ReadAll(progress)
			seen = append(seen, more...)
			return failed("kubectl port-forward exited before it was forwarding")
		default:
		}
		if forwardingReported(seen, localPort) {
			return nil
		}
	}
}

// lastLine returns the last non-empty line of output, shortened to 300 bytes.
func lastLine(output []byte) string {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if len(last) > 300 {
		last = last[:300] + "…"
	}
	return last
}

// forwardingReported reports whether output, what kubectl port-forward has
// written so far, has the line kubectl writes once it listens on
// 127.0.0.1:localPort.
func forwardingReported(output []byte, localPort int) bool {
	want := fmt.Sprintf("Forwarding from 127.0.0.1:%d -> ", localPort)
	for line := range strings.Lines(string(output)) {
		if strings.HasPrefix(line, want) {
			return true
		}
	}
	return false
}

// discoverOrkaService finds the Orka API service in the given namespace.
// Tries well-known service names first, then falls back to label selector.
func discoverOrkaService(restConfig *rest.Config, namespace string) string {
	transportConfig, err := restConfig.TransportConfig()
	if err != nil {
		return ""
	}
	rt, err := transport.New(transportConfig)
	if err != nil {
		return ""
	}

	httpClient := &http.Client{Transport: rt, Timeout: 5 * time.Second}

	// Strategy 1: check well-known service names
	for _, candidate := range []string{"orka-api", cliProgramName, "orka-controller-manager"} {
		if checkServiceExists(httpClient, restConfig.Host, namespace, candidate) {
			return candidate
		}
	}

	// Strategy 2: find by label (look for a service with an "api" port)
	if name := findServiceByLabel(httpClient, restConfig.Host, namespace); name != "" {
		return name
	}

	return ""
}

func findServiceByLabel(httpClient *http.Client, host, namespace string) string {
	listURL := fmt.Sprintf("%s/api/v1/namespaces/%s/services?labelSelector=%s",
		host, namespace, orkaServiceLabel)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return ""
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var result struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Ports []struct {
					Name string `json:"name"`
				} `json:"ports"`
			} `json:"spec"`
		} `json:"items"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}

	// Prefer a service with an "api" port
	for _, svc := range result.Items {
		for _, port := range svc.Spec.Ports {
			if port.Name == "api" {
				return svc.Metadata.Name
			}
		}
	}

	if len(result.Items) > 0 {
		return result.Items[0].Metadata.Name
	}

	return ""
}

func checkServiceExists(httpClient *http.Client, host, namespace, name string) bool {
	svcURL := fmt.Sprintf("%s/api/v1/namespaces/%s/services/%s", host, namespace, name)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, svcURL, nil)
	if err != nil {
		return false
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close() //nolint:errcheck

	return resp.StatusCode == http.StatusOK
}

// kubeconfigLoadingRules load the kubeconfig at kubeconfigPath, or the default
// kubeconfig ($KUBECONFIG, then ~/.kube/config) when the path is empty.
func kubeconfigLoadingRules(kubeconfigPath string) *clientcmd.ClientConfigLoadingRules {
	if kubeconfigPath != "" {
		return &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath}
	}
	return clientcmd.NewDefaultClientConfigLoadingRules()
}

// kubeconfigLoader loads the kubeconfig at kubeconfigPath with the context
// named kubeContext, or with its current context when kubeContext is "".
func kubeconfigLoader(kubeconfigPath, kubeContext string) clientcmd.ClientConfig {
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		kubeconfigLoadingRules(kubeconfigPath), &clientcmd.ConfigOverrides{CurrentContext: kubeContext})
}

// kubeconfigContext is the current context of a kubeconfig, read once. Service
// discovery and the kubeconfig bearer token use the kubeconfig as read then,
// and kubectl and the port-forward cache use this context by name, so
// switching contexts while the CLI connects cannot pair one cluster's
// port-forward with another context's credential.
type kubeconfigContext struct {
	path      string // the --kubeconfig flag; "" means $KUBECONFIG or ~/.kube/config
	name      string // "" when the kubeconfig sets no current context
	namespace string // the context's namespace
	// identity is a digest of the context's name and namespace, its cluster and
	// user names, the file the cluster comes from and how the cluster is
	// reached: server, proxy and TLS settings, including the CA it trusts. Two
	// contexts that differ in any of these get separate port-forwards. It is ""
	// when the kubeconfig has no usable current context or its CA file cannot
	// be read, and then no port-forward is cached.
	identity string
	// cluster is a digest of the part of the identity that decides where
	// kubectl connects: the cluster's source file, server, proxy and TLS
	// settings. It is "" whenever identity is.
	cluster string
	// config is the kubeconfig as read, with relative paths resolved, or nil
	// when it sets no current context.
	config *clientcmdapi.Config
}

// currentKubeconfigContext reads the current context of the kubeconfig at
// kubeconfigPath.
func currentKubeconfigContext(kubeconfigPath string) kubeconfigContext {
	return readKubeconfigContext(kubeconfigPath, "")
}

// readKubeconfigContext reads the context called name in the kubeconfig at
// kubeconfigPath, or its current context when name is "".
func readKubeconfigContext(kubeconfigPath, name string) kubeconfigContext {
	kc := kubeconfigContext{path: kubeconfigPath}
	raw, err := kubeconfigLoader(kubeconfigPath, "").RawConfig()
	if name == "" {
		name = raw.CurrentContext
	}
	if err != nil || name == "" {
		return kc
	}
	kc.name = name
	kc.config = &raw
	kubeCtx, ok := raw.Contexts[kc.name]
	if !ok || kubeCtx == nil {
		return kc
	}
	kc.namespace = kubeCtx.Namespace
	cluster, ok := raw.Clusters[kubeCtx.Cluster]
	if !ok || cluster == nil || cluster.Server == "" {
		return kc
	}
	ca, err := clusterCA(cluster)
	if err != nil {
		return kc
	}
	clusterFields := []string{
		resolvedPath(cluster.LocationOfOrigin), cluster.Server, cluster.ProxyURL, cluster.TLSServerName,
		strconv.FormatBool(cluster.InsecureSkipTLSVerify),
		cluster.CertificateAuthority, string(ca),
	}
	clusterSum := sha256.Sum256([]byte(strings.Join(clusterFields, "\x00")))
	kc.cluster = hex.EncodeToString(clusterSum[:])
	sum := sha256.Sum256([]byte(strings.Join(append([]string{
		kc.name, kubeCtx.Namespace, kubeCtx.Cluster, kubeCtx.AuthInfo,
	}, clusterFields...), "\x00")))
	kc.identity = hex.EncodeToString(sum[:])
	return kc
}

// clusterCA returns the CA that clients of cluster trust: its
// certificate-authority-data, which takes precedence as in client-go, or else
// the contents of its certificate-authority file, which can be replaced without
// changing the kubeconfig.
func clusterCA(cluster *clientcmdapi.Cluster) ([]byte, error) {
	if len(cluster.CertificateAuthorityData) > 0 || cluster.CertificateAuthority == "" {
		return cluster.CertificateAuthorityData, nil
	}
	return os.ReadFile(cluster.CertificateAuthority)
}

// clientConfig is kc's context in the kubeconfig as read, whatever the file
// holds now. With no context it loads the kubeconfig with client-go's
// fallbacks, such as in-cluster configuration.
func (kc kubeconfigContext) clientConfig() clientcmd.ClientConfig {
	if kc.config == nil {
		return kubeconfigLoader(kc.path, kc.name)
	}
	return clientcmd.NewNonInteractiveClientConfig(*kc.config, kc.name, &clientcmd.ConfigOverrides{}, kubeconfigLoadingRules(kc.path))
}

// token returns the bearer token of kc's user.
func (kc kubeconfigContext) token() (string, error) {
	if kc.name == "" {
		return "", fmt.Errorf("no current context set in kubeconfig")
	}
	config := kc.clientConfig()
	rawConfig, err := config.RawConfig()
	if err != nil {
		return "", fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	ctx, ok := rawConfig.Contexts[kc.name]
	if !ok || ctx == nil {
		return "", fmt.Errorf("context %q not found in kubeconfig", kc.name)
	}

	authInfo, ok := rawConfig.AuthInfos[ctx.AuthInfo]
	if !ok || authInfo == nil {
		return "", fmt.Errorf("user %q not found in kubeconfig", ctx.AuthInfo)
	}

	if authInfo.Token != "" {
		return authInfo.Token, nil
	}

	if authInfo.TokenFile != "" {
		data, err := os.ReadFile(authInfo.TokenFile)
		if err != nil {
			return "", fmt.Errorf("failed to read token file %q: %w", authInfo.TokenFile, err)
		}
		// client-go trims the file the same way.
		return strings.TrimSpace(string(data)), nil
	}

	if authInfo.Exec != nil {
		restConfig, err := config.ClientConfig()
		if err != nil {
			return "", fmt.Errorf("failed to get REST config for exec-based auth: %w", err)
		}
		if restConfig.BearerToken != "" {
			return restConfig.BearerToken, nil
		}
	}

	if authInfo.AuthProvider != nil {
		restConfig, err := config.ClientConfig()
		if err != nil {
			return "", fmt.Errorf("failed to get REST config for auth provider: %w", err)
		}
		if restConfig.BearerToken != "" {
			return restConfig.BearerToken, nil
		}
	}

	return "", fmt.Errorf("no token, token file, exec, or auth provider found for user %q", ctx.AuthInfo)
}

// kubeTarget is what the CLI connects through when no server is named: a
// kubeconfig context and the namespace the user selected, if any.
type kubeTarget struct {
	kubeconfigContext
	// selectedNamespace is --namespace, ORKA_NAMESPACE or the config file's
	// namespace, or "" when none is set.
	selectedNamespace string
}

// discoveryNamespaces are the namespaces discovery searches for the Orka
// Service, in order: the selected namespace, the context's namespace, then the
// namespaces Orka is usually installed in.
func (t kubeTarget) discoveryNamespaces() []string {
	var namespaces []string
	for _, ns := range []string{t.selectedNamespace, t.namespace, "orka-system", cliProgramName, defaultNamespace} {
		if ns != "" && !slices.Contains(namespaces, ns) {
			namespaces = append(namespaces, ns)
		}
	}
	return namespaces
}

// cacheKey names t's port-forward cache entry: its context's identity, and the
// selected namespace when one is set, since both change which Service
// discovery finds. It is "" when the context has no identity.
func (t kubeTarget) cacheKey() string {
	if t.identity == "" || t.selectedNamespace == "" {
		return t.identity
	}
	sum := sha256.Sum256([]byte(t.identity + "\x00" + t.selectedNamespace))
	return hex.EncodeToString(sum[:])
}

// resolvedPath returns path as an absolute path with symlinks resolved, so
// one file has one name however it was given; it returns path unchanged when
// it cannot be resolved.
func resolvedPath(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	return path
}

// configPath returns the full path to the config file.
func configPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, configDir, configFile)
}

// loadConfig reads the config file. Returns empty config on error.
func loadConfig() orkaConfig {
	var cfg orkaConfig
	path := configPath()
	if path == "" {
		return cfg
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	_ = yaml.Unmarshal(data, &cfg)
	return cfg
}

// saveConfig writes the config file with 0600 permissions.
func saveConfig(cfg orkaConfig) error {
	path := configPath()
	if path == "" {
		return fmt.Errorf("cannot determine home directory")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	// A private temporary file renamed over the config, which os.Rename
	// replaces on every platform: a concurrent reader never sees a partial
	// config, and on Unix sees the old one or the new one.
	file, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("save config: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	return os.Rename(file.Name(), path)
}

// maskToken shows only the first 4 chars + *** for security.
func maskToken(token string) string {
	if len(token) <= 4 {
		return "***"
	}
	return token[:4] + "...***"
}
