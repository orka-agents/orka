/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const (
	kagentLockSuffix         = "-kagent-lock"
	kagentRecoveryAnnotation = "orka.ai/agent-runtime-recovery-uid"
	// kagentDeployAnnotation carries the nonce of the deploy that created a
	// relay Service or policy.
	kagentDeployAnnotation = "orka.ai/kagent-deploy"
	kagentFieldOwner       = "orka-agent-runtime-kagent"
	kagentControllerEpoch  = "orka-controller"
	kagentAdapterDigestEnv = "ORKA_ACP_KAGENT_ADAPTER_DIGEST"
	kagentEpochEnv         = "ORKA_ACP_CONTROLLER_EPOCH"
	kagentOrkaAPIEnv       = "ORKA_ACP_ARTIFACT_API_URL"
	kagentRollbackTimeout  = time.Minute
	kagentPatchTest        = "test"
	kagentPatchReplace     = "replace"
	kagentSpecPath         = "/spec"
	kagentSelectorPath     = "/spec/selector"
)

// kagentRuntimeOptions are the settings of deploy, activate, and retire.
type kagentRuntimeOptions struct {
	Context                string
	ControllerNamespace    string
	Image                  string
	ModelUpstreamTokenFile string
	StorageClass           string
	OrkaAPIURL             string
	SubstrateNamespace     string
	ReadyTimeout           time.Duration
	RetireTimeout          time.Duration
}

// kagentRuntimes deploys, activates, and retires the runtimes of wrapped
// kagent Agents from the bundles "orka agent import kagent --bundle-dir"
// writes. Every command holds a lock per Agent it changes, so runs for one
// Agent never overlap.
type kagentRuntimes struct {
	c            client.Client
	capabilities func(ctx context.Context, namespace, service string) ([]byte, error)
	resolve      func(ctx context.Context, image string) (pinned, adapter string, err error)
	importer     func(ctx context.Context, args []string, quiet bool) error
	namespace    string
	options      kagentRuntimeOptions
	out          io.Writer
	holder       string
	poll         time.Duration
	locks        []*corev1.ConfigMap
}

func newAgentRuntimeKagentCmd() *cobra.Command {
	var options kagentRuntimeOptions
	cmd := &cobra.Command{
		Use:   kagentProviderKind,
		Short: "Deploy, activate, and retire the runtimes of wrapped kagent Agents",
		Long: `Run wrapped kagent Agents from the deployment bundles that
"orka agent import kagent --dry-run --bundle-dir <dir>" writes. A bundle names
its runtime after the configuration it pins, so re-pinning an Agent, after
kagent recompiles it or after an upgrade, deploys a new runtime beside the old
one, which keeps serving its sessions until it is retired:

  1. deploy    the new runtime, and wait until Orka reports it Ready;
  2. activate  it, pointing the Orka Agent at it;
  3. retire    the Agent's older runtimes.

These commands work in --namespace, by default orka-system, where the
runtimes, their relays, and by default the Orka controller run; unlike other
orka commands, they never fall back to the namespace orka config or kubeconfig
names. They need Kubernetes access to it (--kubeconfig and --context) and, for
activate, the Orka API, which activate finds like "orka agent import kagent":
through kubeconfig's current context, unless --server and --token name it.
Each command holds a lock per Agent it changes, the ConfigMap
<agent>-kagent-lock; a run that was killed leaves its lock, which you delete
once no run is left.`,
	}
	cmd.PersistentFlags().StringVar(&options.Context, "context", "", "kubeconfig context (default: the current context)")
	cmd.AddCommand(newKagentDeployCmd(&options), newKagentActivateCmd(&options), newKagentRetireCmd(&options))
	return cmd
}

func newKagentDeployCmd(options *kagentRuntimeOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy <bundle>",
		Short: "Deploy and register the runtime of one bundle",
		Long: `Deploy the runtime of <bundle>, a directory the import wrote
(<bundle-dir>/<agent>), register it with Orka under Kubernetes recovery, and
wait until Orka reports it Ready.

The runtime runs Orka's kagent runtime image, by default the one released
with this CLI, and mounts the bundle's kagent.json from a ConfigMap. A
deployed runtime keeps its image; to run another, re-pin under a new
--runtime-revision.

A governed Agent needs --model-upstream-token-file, Orka's credential for its
model upstream, which the runtime keeps. Its relay moves to the new runtime
before registration, because Orka's conformance prompt calls it, and back if
deploy fails before the runtime is Ready. Until activation, governed turns on
the old runtime fail their tool and model calls.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			k, ctx, stop, err := newKagentRuntimes(cmd, *options, "deploy")
			if err != nil {
				return err
			}
			defer stop()
			return k.deploy(ctx, args[0])
		},
	}
	cmd.Flags().StringVar(&options.Image, "image", "", "Orka kagent runtime image, by tag or digest (default: the image released with this CLI)")
	cmd.Flags().StringVar(&options.ModelUpstreamTokenFile, "model-upstream-token-file", "", "File holding Orka's credential for a governed Agent's model upstream")
	cmd.Flags().StringVar(&options.ControllerNamespace, "controller-namespace", "", "Namespace of the Orka controller (default: --namespace)")
	cmd.Flags().StringVar(&options.StorageClass, "storage-class", "", "StorageClass of the broker ledger volume (default: the cluster default)")
	cmd.Flags().StringVar(&options.OrkaAPIURL, "orka-api-url", "", "Orka API base URL the supervisor calls (default http://orka-api.<namespace>.svc:8080)")
	cmd.Flags().StringVar(&options.SubstrateNamespace, "substrate-namespace", "ate-system", "Namespace of kagent v1's Substrate egress, which carries the Agent's calls to the relay")
	cmd.Flags().DurationVar(&options.ReadyTimeout, "ready-timeout", 5*time.Minute, "How long to wait for the runtime to be Ready")
	return cmd
}

func newKagentActivateCmd(options *kagentRuntimeOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "activate <bundle> -- <import flags>",
		Short: "Point the Orka Agents at their deployed runtimes",
		Long: `Point the Orka Agents at their runtimes by running the import again
without --dry-run, with the flags that wrote the bundles, after --. The command
passes --bundle-dir, the directory of <bundle>, itself. It switches every Agent
in the import's input, and only if all of their runtimes are Ready, so deploy
them all first. It refuses while the relay of a governed Agent bundled beside
<bundle> selects another runtime. The import creates the Agents in the runtimes'
namespace, so pass -n before --, and the Orka API and token flags too, which
activate checks before running the import.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if dash := cmd.ArgsLenAtDash(); dash != 1 {
				return fmt.Errorf("activate takes <bundle>, then -- and the import flags")
			}
			if kagentFlag(cmd, "txn-token-file") == "-" && kagentReadsStdin(args[1:]) {
				return fmt.Errorf("--txn-token-file and the import's -f cannot both read standard input")
			}
			k, ctx, stop, err := newKagentRuntimes(cmd, *options, "activate")
			if err != nil {
				return err
			}
			defer stop()
			return k.activate(ctx, args[0], args[1:], cmd.InOrStdin())
		},
	}
}

func newKagentRetireCmd(options *kagentRuntimeOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "retire <bundle>",
		Short: "Delete the Agent's other runtimes",
		Long: `Delete every runtime of the bundle's Agent and kagent Agent except the
bundle's own, which the Orka Agent must use. Orka drains each one first, so
this waits until its sessions are done. It refuses while a governed Agent's
relay selects another runtime. Running it again finishes an earlier retire
that stopped: it also finds runtimes whose registration is gone but whose
objects are left.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			k, ctx, stop, err := newKagentRuntimes(cmd, *options, "retire")
			if err != nil {
				return err
			}
			defer stop()
			return k.retire(ctx, args[0])
		},
	}
	cmd.Flags().DurationVar(&options.RetireTimeout, "retire-timeout", 30*time.Minute, "How long to wait for each old runtime to drain")
	return cmd
}

// newKagentRuntimes connects to the cluster. An interrupt cancels the
// returned context, so a deploy moves its relay back before it exits.
func newKagentRuntimes(cmd *cobra.Command, options kagentRuntimeOptions, command string) (*kagentRuntimes, context.Context, func(), error) {
	namespace, _ := cmd.Flags().GetString(cliNamespaceQuery)
	namespace = cmp.Or(strings.TrimSpace(namespace), "orka-system")
	options.ControllerNamespace = cmp.Or(options.ControllerNamespace, namespace)
	kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: options.Context}).ClientConfig()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	if command == "activate" {
		if err := kagentAPIContextProblem(cmd, rules, options.Context); err != nil {
			return nil, nil, nil, err
		}
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, nil, nil, err
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		return nil, nil, nil, err
	}
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, nil, err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, nil, err
	}
	// Resolved before any lock is taken, so a token file that cannot be read
	// fails the run cleanly.
	txnToken := ""
	if command == "activate" {
		if txnToken, err = kagentTxnToken(cmd); err != nil {
			return nil, nil, nil, err
		}
	}
	host, _ := os.Hostname()
	k := &kagentRuntimes{
		c: c, namespace: namespace, options: options, out: cmd.ErrOrStderr(), poll: 2 * time.Second,
		holder: fmt.Sprintf("%s pid %d on %s", command, os.Getpid(), host),
		capabilities: func(ctx context.Context, namespace, service string) ([]byte, error) {
			// The API server's service proxy reaches the runtime's public endpoint.
			return clientset.CoreV1().Services(namespace).ProxyGet("http", service, kagentControlPortName, "/v2/capabilities", nil).DoRaw(ctx)
		},
		resolve: func(ctx context.Context, image string) (string, string, error) {
			if image == "" {
				var err error
				if image, err = kagentDefaultImage(version); err != nil {
					return "", "", err
				}
			}
			return resolveKagentRuntimeImage(ctx, image, nil)
		},
		importer: func(ctx context.Context, args []string, quiet bool) error {
			return runKagentImportCmd(ctx, cmd, append(args, "-n", namespace), txnToken, quiet)
		},
	}
	ctx, stop := kagentSignals(cmd.Context())
	return k, ctx, func() { k.unlock(); stop() }, nil
}

// kagentAPIContextProblem refuses an activate whose --context is not
// kubeconfig's current context unless --server and --token name the Orka
// API: the import finds the API, and its fallback token, through the current
// context, so it would reach another cluster than the locks and relays.
func kagentAPIContextProblem(cmd *cobra.Command, rules *clientcmd.ClientConfigLoadingRules, selected string) error {
	if selected == "" || (kagentFlag(cmd, "server") != "" && kagentFlag(cmd, "token") != "") {
		return nil
	}
	raw, err := rules.Load()
	if err != nil {
		return fmt.Errorf("loading kubeconfig: %w", err)
	}
	if raw.CurrentContext == selected {
		return nil
	}
	return fmt.Errorf("activate reaches the Orka API through kubeconfig's current context %q, not --context %q; switch to that context, or pass its Orka API with --server and --token", raw.CurrentContext, selected)
}

// kagentSignals cancels the returned context on an interrupt, so a deploy
// moves its relay back before it exits. A write to a closed output, such as
// a pipe whose reader the same interrupt stopped, must not end the run
// before that, so SIGPIPE is ignored and the write fails instead.
func kagentSignals(parent context.Context) (context.Context, context.CancelFunc) {
	signal.Ignore(syscall.SIGPIPE)
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
}

// kagentTxnToken resolves the transaction token the import sends, reading
// --txn-token-file here: the import's client exits the process when it cannot
// read the file, which would skip releasing the locks.
func kagentTxnToken(cmd *cobra.Command) (string, error) {
	token, file := kagentFlag(cmd, "txn-token"), kagentFlag(cmd, "txn-token-file")
	if token != "" || file == "" {
		return token, nil
	}
	data, err := readFileOrStdin(file)
	if err != nil {
		return "", fmt.Errorf("reading --txn-token-file: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// kagentFlag reads a flag of cmd or of a parent, parsed or not.
func kagentFlag(cmd *cobra.Command, name string) string {
	if flag := cmd.Flag(name); flag != nil {
		return flag.Value.String()
	}
	return ""
}

// runKagentImportCmd runs "orka agent import kagent" with args, and with the
// Orka API flags this command was given and the transaction token it resolved.
func runKagentImportCmd(ctx context.Context, cmd *cobra.Command, args []string, txnToken string, quiet bool) error {
	forwarded := strings.Fields("agent import kagent")
	for _, name := range []string{"server", "token", "kubeconfig"} {
		if flag := cmd.Flags().Lookup(name); flag != nil && flag.Changed {
			forwarded = append(forwarded, "--"+name, flag.Value.String())
		}
	}
	if txnToken != "" {
		forwarded = append(forwarded, "--txn-token", txnToken)
	}
	root := newRootCmd()
	root.SetArgs(append(forwarded, args...))
	root.SetIn(cmd.InOrStdin())
	root.SetOut(cmd.OutOrStdout())
	root.SetErr(cmd.ErrOrStderr())
	if quiet {
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
	}
	return root.ExecuteContext(ctx)
}

func (k *kagentRuntimes) logf(format string, args ...any) {
	fmt.Fprintf(k.out, format+"\n", args...) //nolint:errcheck
}

// lock holds a lock for each Agent: a ConfigMap, created atomically and
// deleted by unlock.
func (k *kagentRuntimes) lock(ctx context.Context, agents ...string) error {
	for _, agent := range agents {
		lock := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: agent + kagentLockSuffix, Namespace: k.namespace},
			Data: map[string]string{"holder": k.holder}}
		if err := k.c.Create(ctx, lock); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("another deploy, activate, or retire of Agent %s holds ConfigMap %s; if that run is gone, delete it", agent, lock.Name)
			}
			return fmt.Errorf("locking Agent %s: %w", agent, err)
		}
		k.locks = append(k.locks, lock)
	}
	return nil
}

// unlock deletes the locks this run created, and only those.
func (k *kagentRuntimes) unlock() {
	ctx, cancel := context.WithTimeout(context.Background(), kagentRollbackTimeout)
	defer cancel()
	for _, lock := range k.locks {
		err := k.c.Delete(ctx, lock, client.Preconditions{UID: &lock.UID})
		if err != nil && !apierrors.IsNotFound(err) {
			k.logf("kagent-runtime: could not delete lock %s: %v", lock.Name, err)
		}
	}
	k.locks = nil
}

func (k *kagentRuntimes) key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: k.namespace, Name: name}
}

// getOptional reads an object, reporting found=false only for NotFound.
func (k *kagentRuntimes) getOptional(ctx context.Context, name string, obj client.Object) (bool, error) {
	switch err := k.c.Get(ctx, k.key(name), obj); {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}

// kagentSource is the bundle's kagent Agent as every object of its runtimes,
// and the Orka Agent, names it: "<apiVersion> <namespace>/<name>".
func kagentSource(bundle kagentBundle) string {
	return bundle.Source
}

// relayOwned reports whether a relay object serves the Agent and kagent
// Agent of bundle.
func relayOwned(obj client.Object, bundle kagentBundle) bool {
	return obj.GetLabels()[kagentAgentLabel] == bundle.Agent && obj.GetAnnotations()[kagentSourceAnnotation] == kagentSource(bundle)
}

// kagentRelayRollback is what a deploy changed on its Agent's relay before
// the runtime was Ready, so that a failure can change it back.
type kagentRelayRollback struct {
	service  string
	selector map[string]string
	ports    []corev1.ServicePort
	// createdService and createdPolicy mark the relay Service and policy
	// this deploy creates, by their UID once the answer arrives and by the
	// nonce annotation they carry before.
	createdService types.UID
	serviceNonce   string
	createdPolicy  types.UID
	policyNonce    string
	policy         *networkingv1.NetworkPolicySpec
	applied        *networkingv1.NetworkPolicySpec
}

func (k *kagentRuntimes) deploy(ctx context.Context, dir string) (err error) {
	bundle, config, err := readKagentBundle(dir)
	if err != nil {
		return err
	}
	source := kagentSource(bundle)
	if err := k.lock(ctx, bundle.Agent); err != nil {
		return err
	}
	// One Orka Agent name may come from kagent Agents of the same name in
	// different namespaces; a deploy must not take the runtimes or relay of
	// the one the Orka Agent wraps.
	agent := &corev1alpha1.Agent{}
	found, err := k.getOptional(ctx, bundle.Agent, agent)
	if err != nil {
		return err
	}
	if wraps := agent.Annotations[kagentSourceAnnotation]; found && wraps != "" && wraps != source {
		return fmt.Errorf("the Orka Agent %s wraps kagent Agent %s, not %s; import %s into another Orka namespace", bundle.Agent, wraps, source, source)
	}
	relay := ""
	if bundle.Governed {
		// Both relay objects must be absent, or this Agent's for this kagent
		// Agent: before activation, no Orka Agent records which kagent Agent
		// a deployed relay serves.
		if relay, _, _, err = kagentRelayService(bundle.Relay); err != nil {
			return err
		}
		for kind, obj := range map[string]client.Object{"service": &corev1.Service{}, "networkpolicy": &networkingv1.NetworkPolicy{}} {
			found, err := k.getOptional(ctx, relay, obj)
			if err != nil {
				return err
			}
			if found && !relayOwned(obj, bundle) {
				return fmt.Errorf("%s %s is not %s's relay for kagent Agent %s; give %s its own relay URL, or adopt it by labeling it %s=%s and annotating it %s='%s'",
					kind, relay, bundle.Agent, source, bundle.Agent, kagentAgentLabel, bundle.Agent, kagentSourceAnnotation, source)
			}
		}
	}
	options := kagentRenderOptions{Namespace: k.namespace, StorageClass: k.options.StorageClass,
		OrkaAPIURL: k.options.OrkaAPIURL, SubstrateNamespace: k.options.SubstrateNamespace}
	deployment, err := k.keepDeployed(ctx, bundle, &options)
	if err != nil {
		return err
	}
	if deployment == nil {
		if options.Image, options.AdapterDigest, err = k.resolve(ctx, k.options.Image); err != nil {
			return err
		}
	}
	if err := k.ensureAuthSecret(ctx, bundle); err != nil {
		return err
	}
	// Once the Deployment names its AgentRuntime, Orka's recovery owns its
	// epoch, which it advances only after draining the runtime's sessions:
	// applying a newer one here would restart the pod before that.
	// The apply then holds only while the Deployment is as read, so it never
	// writes back an epoch recovery advanced meanwhile.
	deploymentVersion := ""
	if deployment != nil && deployment.Annotations[kagentRecoveryAnnotation] != "" {
		options.ControllerEpoch, err = deployedKagentEpoch(deployment)
		deploymentVersion = deployment.ResourceVersion
	} else {
		options.ControllerEpoch, err = k.controllerEpoch(ctx)
	}
	if err != nil {
		return err
	}
	options.Part = kagentPartWorkload
	if err := k.applyPart(ctx, bundle, config, options, deploymentVersion); err != nil {
		if apierrors.IsConflict(err) {
			return fmt.Errorf("deployment %s changed since this deploy read it, as when Orka's recovery advances its epoch; deploy again: %w", bundle.Runtime, err)
		}
		return err
	}
	if err := k.waitRollout(ctx, bundle.Runtime); err != nil {
		return err
	}
	rollback := &kagentRelayRollback{service: relay}
	// Until the runtime is Ready, any failure, or an interrupt, moves the
	// Agent's relay back to what it served before.
	defer func() {
		if err != nil {
			k.restoreRelay(bundle, rollback)
		}
	}()
	if bundle.Governed {
		if err := k.handOffRelay(ctx, bundle, config, options, rollback); err != nil {
			return err
		}
	}
	return k.register(ctx, dir, bundle, config, options)
}

// keepDeployed sets in options what a runtime deployed before keeps, since
// changing it restarts the runtime without draining or fails: its ledger's
// StorageClass, and its image and Orka API URL. An omitted flag keeps each;
// another refuses. It returns the runtime's Deployment, or nil for none.
func (k *kagentRuntimes) keepDeployed(ctx context.Context, bundle kagentBundle, options *kagentRenderOptions) (*appsv1.Deployment, error) {
	claim := &corev1.PersistentVolumeClaim{}
	claimed, err := k.getOptional(ctx, bundle.Runtime+"-ledger", claim)
	if err != nil {
		return nil, err
	}
	if claimed && claim.Spec.StorageClassName != nil {
		switch class := *claim.Spec.StorageClassName; {
		case options.StorageClass == "":
			options.StorageClass = class
		case options.StorageClass != class:
			return nil, fmt.Errorf("runtime %s's ledger uses StorageClass %s; deploy under a new --runtime-revision to change --storage-class", bundle.Runtime, class)
		}
	}
	deployment := &appsv1.Deployment{}
	if deployed, err := k.getOptional(ctx, bundle.Runtime, deployment); err != nil || !deployed {
		return nil, err
	}
	if err := kagentOwned(bundle, "Deployment", deployment); err != nil {
		return nil, err
	}
	// The image is part of the runtime's immutable profile.
	if options.Image, options.AdapterDigest, err = deployedKagentImage(deployment); err != nil {
		return nil, err
	}
	switch deployedURL := deployedKagentEnv(deployment, kagentOrkaAPIEnv); {
	case options.OrkaAPIURL == "":
		options.OrkaAPIURL = deployedURL
	case options.OrkaAPIURL != deployedURL:
		return nil, fmt.Errorf("runtime %s calls the Orka API at %s; deploy under a new --runtime-revision to change --orka-api-url", bundle.Runtime, deployedURL)
	}
	k.logf("Runtime %s is already deployed; keeping its image.", bundle.Runtime)
	return deployment, nil
}

// deployedKagentEnv reads a variable a deployed runtime's supervisor runs
// with, empty when it has none.
func deployedKagentEnv(deployment *appsv1.Deployment, name string) string {
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name != kagentSupervisorContainer {
			continue
		}
		for _, env := range container.Env {
			if env.Name == name {
				return env.Value
			}
		}
	}
	return ""
}

// deployedKagentEpoch reads the controller epoch a deployed runtime's
// supervisor runs with.
func deployedKagentEpoch(deployment *appsv1.Deployment) (int64, error) {
	if epoch, err := strconv.ParseInt(deployedKagentEnv(deployment, kagentEpochEnv), 10, 64); err == nil && epoch > 0 {
		return epoch, nil
	}
	return 0, fmt.Errorf("deployment %s does not run a kagent runtime supervisor with a controller epoch", deployment.Name)
}

// deployedKagentImage reads the image and adapter digest a deployed runtime
// runs.
func deployedKagentImage(deployment *appsv1.Deployment) (string, string, error) {
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name != kagentSupervisorContainer {
			continue
		}
		for _, env := range container.Env {
			if env.Name == kagentAdapterDigestEnv && kagentPinnedImage.MatchString(container.Image) && kagentDigest.MatchString(env.Value) {
				return container.Image, env.Value, nil
			}
		}
	}
	return "", "", fmt.Errorf("deployment %s does not run a digest-pinned kagent runtime image; deploy under a new --runtime-revision", deployment.Name)
}

// ensureAuthSecret creates the runtime's auth Secret, bound to one
// registration and endpoint. Its values are random and never printed; a
// governed Agent's upstream credential comes from --model-upstream-token-file.
func (k *kagentRuntimes) ensureAuthSecret(ctx context.Context, bundle kagentBundle) error {
	name := bundle.Runtime + "-auth"
	var upstream []byte
	if bundle.Governed && k.options.ModelUpstreamTokenFile != "" {
		raw, err := os.ReadFile(k.options.ModelUpstreamTokenFile)
		if err != nil {
			return fmt.Errorf("reading --model-upstream-token-file: %w", err)
		}
		// The broker refuses a credential with control characters, such as
		// the newline a file usually ends with.
		upstream = bytes.Map(func(r rune) rune {
			if r == '\r' || r == '\n' {
				return -1
			}
			return r
		}, raw)
		// An empty file is not an omitted flag: a deployed runtime would keep
		// its credential without a comparison.
		if len(upstream) == 0 {
			return fmt.Errorf("--model-upstream-token-file %s is empty", k.options.ModelUpstreamTokenFile)
		}
	}
	secret := &corev1.Secret{}
	found, err := k.getOptional(ctx, name, secret)
	if err != nil {
		return err
	}
	labels := map[string]string{"orka.ai/agent-runtime-auth": cliQueryTrue, "orka.ai/agent-runtime-name": bundle.Runtime,
		kagentRuntimeLabel: bundle.Runtime, kagentAgentLabel: bundle.Agent}
	annotations := map[string]string{"orka.ai/agent-runtime-endpoint": "http://" + bundle.Runtime + "." + k.namespace + ".svc.cluster.local:8080",
		kagentSourceAnnotation: kagentSource(bundle)}
	if !found {
		data := map[string][]byte{}
		for _, key := range []string{"controller-token", "capability-secret", "provider-token"} {
			value := make([]byte, 32)
			if _, err := rand.Read(value); err != nil {
				return err
			}
			data[key] = []byte(hex.EncodeToString(value))
		}
		if bundle.Governed {
			if len(upstream) == 0 {
				return fmt.Errorf("a governed Agent needs --model-upstream-token-file")
			}
			data["model-upstream-token"] = upstream
		}
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: k.namespace, Labels: labels, Annotations: annotations}, Data: data}
		return k.c.Create(ctx, secret)
	}
	// A same-named Secret another owner made holds credentials this runtime
	// must not mount, and its metadata is not this deploy's to change.
	if err := kagentOwned(bundle, "Secret", secret); err != nil {
		return err
	}
	if len(upstream) != 0 {
		// The broker reads the credential once, at start, so a runtime keeps it.
		stored, given := sha256.Sum256(secret.Data["model-upstream-token"]), sha256.Sum256(upstream)
		if subtle.ConstantTimeCompare(stored[:], given[:]) != 1 {
			return fmt.Errorf("runtime %s holds another upstream credential than --model-upstream-token-file; rotate it by deploying under a new --runtime-revision", bundle.Runtime)
		}
	}
	// Orka uses the Secret only with this metadata, so a redeploy reasserts it.
	patch, _ := json.Marshal(map[string]any{kagentFieldMetadata: map[string]any{"labels": labels, kagentFieldAnnotations: annotations}})
	return k.c.Patch(ctx, secret, client.RawPatch(types.MergePatchType, patch))
}

// controllerEpoch reads the Orka controller's epoch, which the supervisor
// needs at start.
func (k *kagentRuntimes) controllerEpoch(ctx context.Context) (int64, error) {
	var epochs corev1alpha1.ControllerEpochList
	if err := k.c.List(ctx, &epochs, client.InNamespace(k.options.ControllerNamespace)); err != nil {
		return 0, fmt.Errorf("reading the Orka controller epoch: %w", err)
	}
	var found []int64
	for _, epoch := range epochs.Items {
		if epoch.Spec.Name == kagentControllerEpoch {
			found = append(found, epoch.Status.Epoch)
		}
	}
	if len(found) != 1 || found[0] <= 0 {
		return 0, fmt.Errorf("expected one initialized Orka controller epoch in namespace %s; set --controller-namespace", k.options.ControllerNamespace)
	}
	return found[0], nil
}

// applyPart renders one part and applies it server-side, which keeps the
// fields others set, such as the recovery annotation and the epoch Orka's
// recovery writes. A deploymentVersion makes the runtime Deployment's apply
// hold only while it has that resourceVersion.
func (k *kagentRuntimes) applyPart(ctx context.Context, bundle kagentBundle, config []byte, options kagentRenderOptions, deploymentVersion string) error {
	items, err := renderKagentPart(bundle, config, options)
	if err != nil {
		return err
	}
	for _, item := range items {
		obj, ok := item.(client.Object)
		if !ok {
			return fmt.Errorf("rendered %T is not an object", item)
		}
		if err := k.requireOwned(ctx, bundle, obj); err != nil {
			return err
		}
		if _, ok := obj.(*appsv1.Deployment); ok && deploymentVersion != "" {
			obj.SetResourceVersion(deploymentVersion)
		}
		if err := k.c.Patch(ctx, obj, client.Apply, client.FieldOwner(kagentFieldOwner), client.ForceOwnership); err != nil { //nolint:staticcheck // typed objects apply as before
			return fmt.Errorf("applying %s %s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
		}
	}
	return nil
}

// requireOwned refuses when a same-named object exists that is not this
// runtime's, as rendered: one a failed deploy of another source or a person
// made. Applying would take over its fields.
func (k *kagentRuntimes) requireOwned(ctx context.Context, bundle kagentBundle, obj client.Object) error {
	existing, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("rendered %T is not an object", obj)
	}
	switch err := k.c.Get(ctx, client.ObjectKeyFromObject(obj), existing); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	}
	return kagentOwned(bundle, obj.GetObjectKind().GroupVersionKind().Kind, existing)
}

// kagentOwned refuses existing unless it is the bundle's runtime's.
func kagentOwned(bundle kagentBundle, kind string, existing client.Object) error {
	if existing.GetLabels()[kagentRuntimeLabel] != bundle.Runtime || existing.GetAnnotations()[kagentSourceAnnotation] != kagentSource(bundle) {
		return fmt.Errorf("%s %s/%s exists but is not runtime %s's for kagent Agent %s; delete it, or deploy under a new --runtime-revision",
			kind, existing.GetNamespace(), existing.GetName(), bundle.Runtime, kagentSource(bundle))
	}
	return nil
}

// waitFor polls check until it reports done, fails, or timeout passes.
// The deadline bounds every read check makes too, so a stalled read cannot
// outlast it.
func (k *kagentRuntimes) waitFor(ctx context.Context, timeout time.Duration, what string, check func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		done, err := check(ctx)
		if err != nil || done {
			return err
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("timed out after %s waiting for %s", timeout, what)
			}
			return ctx.Err()
		case <-time.After(k.poll):
		}
	}
}

func (k *kagentRuntimes) waitRollout(ctx context.Context, name string) error {
	return k.waitFor(ctx, k.options.ReadyTimeout, "deployment "+name+" to roll out", func(ctx context.Context) (bool, error) {
		deployment := &appsv1.Deployment{}
		if err := k.c.Get(ctx, k.key(name), deployment); err != nil {
			return false, err
		}
		want := int32(1)
		if deployment.Spec.Replicas != nil {
			want = *deployment.Spec.Replicas
		}
		status := deployment.Status
		return status.ObservedGeneration >= deployment.Generation && status.UpdatedReplicas == want &&
			status.Replicas == want && status.AvailableReplicas == want, nil
	})
}

// handOffRelay moves a governed Agent's relay to this runtime. Orka's
// conformance prompt reaches kagent, which calls the relay, so the relay must
// reach this runtime before registration.
func (k *kagentRuntimes) handOffRelay(ctx context.Context, bundle kagentBundle, config []byte, options kagentRenderOptions, rollback *kagentRelayRollback) error {
	options.Part = kagentPartRelay
	items, err := renderKagentPart(bundle, config, options)
	if err != nil {
		return err
	}
	var service *corev1.Service
	var policy *networkingv1.NetworkPolicy
	for _, item := range items {
		switch obj := item.(type) {
		case *corev1.Service:
			service = obj
		case *networkingv1.NetworkPolicy:
			policy = obj
		}
	}
	want := map[string]string{kagentRuntimeLabel: bundle.Runtime}
	current := &corev1.Service{}
	found, err := k.getOptional(ctx, rollback.service, current)
	if err != nil {
		return err
	}
	if !found {
		// Create, not apply: a deploy that created it meanwhile makes this
		// fail. A rollback deletes it, as it does a policy this deploy creates.
		if rollback.serviceNonce, err = kagentNonce(); err != nil {
			return err
		}
		service.Annotations = maps.Clone(service.Annotations)
		service.Annotations[kagentDeployAnnotation] = rollback.serviceNonce
		if err := k.c.Create(ctx, service); err != nil {
			return fmt.Errorf("creating relay %s: %w", rollback.service, err)
		}
		rollback.createdService = service.UID
	} else if !maps.Equal(current.Spec.Selector, want) {
		// One patch that holds only while the Service still selects what it
		// did and is labeled for this Agent, so it never writes back a
		// selector another deploy replaced, nor takes another Agent's relay.
		// It replaces the selector outright, since a merge with an older one
		// would select nothing. Armed before the patch, since an error may
		// follow a committed one.
		// The patch decodes its answer into current, so the rollback keeps a copy.
		before := current.DeepCopy()
		rollback.selector, rollback.ports = before.Spec.Selector, before.Spec.Ports
		ops := append(kagentOwnerTests(bundle),
			kagentPatchOp{kagentPatchTest, kagentSelectorPath, orEmpty(before.Spec.Selector)},
			kagentPatchOp{kagentPatchReplace, kagentSelectorPath, want},
			kagentPatchOp{kagentPatchReplace, "/spec/ports", service.Spec.Ports})
		if err := k.jsonPatch(ctx, current, ops); err != nil {
			return fmt.Errorf("relay %s did not take the handoff: it changed while this deploy ran, or the patch failed; deploy again: %w", rollback.service, err)
		}
	}
	// The relay is this Agent's now, so its policy is too. An existing policy
	// changes only in one tested patch, which replaces its whole spec, so a
	// rollback's fence is exactly the spec it wrote.
	existing := &networkingv1.NetworkPolicy{}
	if found, err = k.getOptional(ctx, rollback.service, existing); err != nil {
		return err
	}
	if !found {
		// A rollback deletes a policy this deploy created. Armed before the
		// create, since an error may follow a committed one: until the answer
		// names its UID, the policy is the one carrying this deploy's nonce.
		if rollback.policyNonce, err = kagentNonce(); err != nil {
			return err
		}
		policy.Annotations = maps.Clone(policy.Annotations)
		policy.Annotations[kagentDeployAnnotation] = rollback.policyNonce
		if err := k.c.Create(ctx, policy); err != nil {
			return fmt.Errorf("creating relay policy %s: %w", rollback.service, err)
		}
		rollback.createdPolicy = policy.UID
		return nil
	}
	if equalJSON(existing.Spec, policy.Spec) {
		return nil
	}
	// Armed even when the selector stays, since the policy alone can shut out
	// the serving runtime's callers, and before the patch, since an error may
	// follow a committed one. The fence is the rendered spec, which sets every
	// field the API server would default.
	rollback.policy, rollback.applied = existing.Spec.DeepCopy(), policy.Spec.DeepCopy()
	ops := append(kagentOwnerTests(bundle), kagentPatchOp{kagentPatchTest, kagentSpecPath, rollback.policy}, kagentPatchOp{kagentPatchReplace, kagentSpecPath, policy.Spec})
	if err := k.jsonPatch(ctx, existing, ops); err != nil {
		return fmt.Errorf("relay policy %s changed while this deploy ran, or the patch failed; deploy again: %w", rollback.service, err)
	}
	// Once the answer arrives, the fence is the spec the API server stored.
	rollback.applied = existing.Spec.DeepCopy()
	return nil
}

// kagentNonce marks an object a deploy creates until the create's answer
// names its UID.
func kagentNonce() (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return hex.EncodeToString(nonce), nil
}

// deleteCreated deletes obj, as read, if this deploy created it: it has the
// created UID, or before the create's answer arrived, carries the deploy's
// nonce. The delete holds only while obj is unchanged since the read.
func (k *kagentRuntimes) deleteCreated(ctx context.Context, obj client.Object, created types.UID, nonce string) error {
	if obj.GetUID() != created && (created != "" || obj.GetAnnotations()[kagentDeployAnnotation] != nonce) {
		return nil
	}
	uid, version := obj.GetUID(), obj.GetResourceVersion()
	if err := k.c.Delete(ctx, obj, client.Preconditions{UID: &uid, ResourceVersion: &version}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

type kagentPatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

func kagentOwnerTests(bundle kagentBundle) []kagentPatchOp {
	return []kagentPatchOp{
		{kagentPatchTest, "/metadata/labels/" + jsonPointerEscape(kagentAgentLabel), bundle.Agent},
		{kagentPatchTest, "/metadata/annotations/" + jsonPointerEscape(kagentSourceAnnotation), kagentSource(bundle)},
	}
}

func jsonPointerEscape(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

func orEmpty(selector map[string]string) map[string]string {
	if selector == nil {
		return map[string]string{}
	}
	return selector
}

func equalJSON(a, b any) bool {
	left, err1 := json.Marshal(a)
	right, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(left, right)
}

func (k *kagentRuntimes) jsonPatch(ctx context.Context, obj client.Object, ops []kagentPatchOp) error {
	patch, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	return k.c.Patch(ctx, obj, client.RawPatch(types.JSONPatchType, patch))
}

// restoreRelay moves a relay back, but only while it still is what this
// deploy made it: a concurrent deploy of the Agent may have moved it since.
func (k *kagentRuntimes) restoreRelay(bundle kagentBundle, rollback *kagentRelayRollback) {
	ctx, cancel := context.WithTimeout(context.Background(), kagentRollbackTimeout)
	defer cancel()
	// A deploy that took the relay since relies on its policy and on what this
	// one created, so those are deleted or restored only while the relay
	// still selects this runtime.
	relay := &corev1.Service{}
	found, err := k.getOptional(ctx, rollback.service, relay)
	if err != nil {
		k.logf("kagent-runtime: could not read relay %s: %v", rollback.service, err)
	}
	serving := found && maps.Equal(relay.Spec.Selector, map[string]string{kagentRuntimeLabel: bundle.Runtime})
	if rollback.serviceNonce != "" && serving {
		k.logf("Runtime %s is not Ready; deleting relay %s, which this deploy created.", bundle.Runtime, rollback.service)
		if err := k.deleteCreated(ctx, relay, rollback.createdService, rollback.serviceNonce); err != nil {
			k.logf("kagent-runtime: could not delete relay %s, which this deploy created: %v", rollback.service, err)
		}
	}
	if rollback.selector != nil {
		k.logf("Runtime %s is not Ready; moving relay %s back.", bundle.Runtime, rollback.service)
		service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: rollback.service, Namespace: k.namespace}}
		ops := []kagentPatchOp{
			{kagentPatchTest, kagentSelectorPath, map[string]string{kagentRuntimeLabel: bundle.Runtime}},
			{kagentPatchReplace, kagentSelectorPath, rollback.selector},
			{kagentPatchReplace, "/spec/ports", rollback.ports},
		}
		if err := k.jsonPatch(ctx, service, ops); err != nil {
			k.logf("kagent-runtime: relay %s was not moved back; it does not select %s, or the patch failed: %v", rollback.service, bundle.Runtime, err)
		}
	}
	policy := &networkingv1.NetworkPolicy{}
	switch {
	case rollback.policyNonce != "" && serving:
		found, err := k.getOptional(ctx, rollback.service, policy)
		if err == nil && found {
			err = k.deleteCreated(ctx, policy, rollback.createdPolicy, rollback.policyNonce)
		}
		if err != nil {
			k.logf("kagent-runtime: could not delete relay policy %s, which this deploy created: %v", rollback.service, err)
		}
	case rollback.policy != nil && rollback.applied != nil && serving:
		// A patch that failed before it committed left the policy as it was.
		if found, err := k.getOptional(ctx, rollback.service, policy); err == nil && found && equalJSON(policy.Spec, *rollback.policy) {
			break
		}
		policy = &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: rollback.service, Namespace: k.namespace}}
		ops := []kagentPatchOp{{kagentPatchTest, kagentSpecPath, rollback.applied}, {kagentPatchReplace, kagentSpecPath, rollback.policy}}
		if err := k.jsonPatch(ctx, policy, ops); err != nil {
			k.logf("kagent-runtime: relay policy %s was not restored; it changed since this deploy applied it, or the patch failed: %v", rollback.service, err)
		}
	}
	*rollback = kagentRelayRollback{service: rollback.service}
}

// register registers the runtime and waits until Orka admits Tasks to it.
func (k *kagentRuntimes) register(ctx context.Context, dir string, bundle kagentBundle, config []byte, options kagentRenderOptions) error {
	// The relay already serves this runtime, so a read that never answers
	// must still reach the rollback.
	capabilitiesCtx, cancel := context.WithTimeout(ctx, k.options.ReadyTimeout)
	capabilities, err := k.capabilities(capabilitiesCtx, k.namespace, bundle.Runtime)
	cancel()
	if err != nil {
		return fmt.Errorf("reading runtime %s's capabilities: %w", bundle.Runtime, err)
	}
	deployment := &appsv1.Deployment{}
	if err := k.c.Get(ctx, k.key(bundle.Runtime), deployment); err != nil {
		return err
	}
	options.Part, options.CapabilitiesJSON, options.DeploymentUID = kagentPartRegistration, capabilities, string(deployment.UID)
	if err := k.applyPart(ctx, bundle, config, options, ""); err != nil {
		return err
	}
	registration := &corev1alpha1.AgentRuntime{}
	if err := k.c.Get(ctx, k.key(bundle.Runtime), registration); err != nil {
		return err
	}
	// Orka recovers the Deployment, restarting it after an Orka controller
	// restart, once the Deployment names the AgentRuntime.
	switch owner := deployment.Annotations[kagentRecoveryAnnotation]; owner {
	case "":
		patch, _ := json.Marshal(map[string]any{kagentFieldMetadata: map[string]any{kagentFieldAnnotations: map[string]string{kagentRecoveryAnnotation: string(registration.UID)}}})
		if err := k.c.Patch(ctx, deployment, client.RawPatch(types.MergePatchType, patch)); err != nil {
			return err
		}
	case string(registration.UID):
	default:
		return fmt.Errorf("deployment %s is recovered by another AgentRuntime (%s)", bundle.Runtime, owner)
	}
	// Activation requires this registration, so it cannot switch Agents
	// through an Orka API in another cluster.
	if err := os.WriteFile(filepath.Join(dir, kagentBundleRegistrationFile), []byte(string(registration.UID)+"\n"), 0o644); err != nil { //nolint:gosec // a UID, not a secret
		return err
	}
	err = k.waitFor(ctx, k.options.ReadyTimeout, "runtime "+bundle.Runtime+" to be Ready", func(ctx context.Context) (bool, error) {
		current := &corev1alpha1.AgentRuntime{}
		if err := k.c.Get(ctx, k.key(bundle.Runtime), current); err != nil {
			return false, err
		}
		// Ready alone stays set while a deleted runtime drains: require what
		// Orka admits Tasks to, the registration this deploy annotated.
		if current.UID != registration.UID || current.DeletionTimestamp != nil {
			return false, fmt.Errorf("runtime %s is being deleted or is not the registration this deploy made; deploy under a new --runtime-revision", bundle.Runtime)
		}
		return current.Status.Ready && current.Status.ObservedGeneration == current.Generation, nil
	})
	if err != nil {
		return err
	}
	k.logf("Runtime %s is Ready. Activate it with: orka agent-runtime kagent activate %s -- <the import flags>", bundle.Runtime, dir)
	return nil
}

// requireRelay refuses unless a governed bundle's relay selects the bundle's
// runtime, which otherwise cannot make its tool and model calls.
func (k *kagentRuntimes) requireRelay(ctx context.Context, dir string, bundle kagentBundle) error {
	if !bundle.Governed {
		return nil
	}
	relay, _, _, err := kagentRelayService(bundle.Relay)
	if err != nil {
		return err
	}
	service := &corev1.Service{}
	found, err := k.getOptional(ctx, relay, service)
	if err != nil {
		return err
	}
	if want := map[string]string{kagentRuntimeLabel: bundle.Runtime}; !found || !maps.Equal(service.Spec.Selector, want) {
		return fmt.Errorf("relay %s selects %v, not runtime %s; move it with: orka agent-runtime kagent deploy %s", relay, service.Spec.Selector, bundle.Runtime, dir)
	}
	return nil
}

// agentRuntimeRef reads the runtime an Orka Agent uses.
func (k *kagentRuntimes) agentRuntimeRef(ctx context.Context, name string) (string, error) {
	agent := &corev1alpha1.Agent{}
	if err := k.c.Get(ctx, k.key(name), agent); err != nil {
		return "", err
	}
	if agent.Spec.Runtime == nil || agent.Spec.Runtime.RuntimeRef == nil {
		return "", nil
	}
	return agent.Spec.Runtime.RuntimeRef.Name, nil
}

func (k *kagentRuntimes) activate(ctx context.Context, dir string, flags []string, stdin io.Reader) error {
	bundle, _, err := readKagentBundle(dir)
	if err != nil {
		return err
	}
	bundleDir, err := filepath.Abs(filepath.Dir(filepath.Clean(dir)))
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "orka-kagent-activate-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work) //nolint:errcheck
	args, err := kagentActivateArgs(ctx, flags, bundleDir, work, stdin)
	if err != nil {
		return err
	}
	// The import switches every Agent in its input, which wrote the bundles
	// beside this one, and refuses unless Orka reports all of their runtimes
	// Ready. A governed Agent's relay may have moved since, to a newer runtime.
	entries, err := os.ReadDir(bundleDir)
	if err != nil {
		return err
	}
	var siblings []kagentBundle
	var dirs []string
	for _, entry := range entries {
		sibling := filepath.Join(bundleDir, entry.Name())
		if _, err := os.Stat(filepath.Join(sibling, kagentBundleRuntimeFile)); !entry.IsDir() || err != nil {
			continue
		}
		b, _, err := readKagentBundle(sibling)
		if err != nil {
			return err
		}
		siblings, dirs = append(siblings, b), append(dirs, sibling)
	}
	names := make([]string, 0, len(siblings))
	for _, sibling := range siblings {
		names = append(names, sibling.Agent)
	}
	if err := k.lock(ctx, names...); err != nil {
		return err
	}
	for i, sibling := range siblings {
		if err := k.requireRelay(ctx, dirs[i], sibling); err != nil {
			return err
		}
	}
	// The import must include this bundle's Agent, or it would switch the
	// others and leave this one: a dry run into a scratch directory shows
	// which Agents these flags import, without touching the bundles.
	preview := filepath.Join(work, "preview")
	if err := k.importer(ctx, append(slices.Clone(args), "--dry-run", "--bundle-dir", preview), true); err != nil {
		return fmt.Errorf("previewing the import: %w", err)
	}
	if _, err := os.Stat(filepath.Join(preview, bundle.Agent, kagentBundleRuntimeFile)); err != nil {
		return fmt.Errorf("the import flags do not import Agent %s; check -f", bundle.Agent)
	}
	if err := k.importer(ctx, append(args, "--bundle-dir", bundleDir), false); err != nil {
		return err
	}
	if ref, err := k.agentRuntimeRef(ctx, bundle.Agent); err != nil || ref != bundle.Runtime {
		return fmt.Errorf("agent %s does not use %s; were the import flags the ones that wrote this bundle? (%v)", bundle.Agent, bundle.Runtime, err)
	}
	k.logf("Agent %s uses %s. Retire its other runtimes with: orka agent-runtime kagent retire %s", bundle.Agent, bundle.Runtime, dir)
	return nil
}

// kagentActivateArgs checks the import flags activate forwards: it refuses
// --dry-run and --txn-token-file, drops a --bundle-dir that names the bundle's own directory, and
// reads standard input (-f -) once, so the preview and the import see the
// same input.
func kagentActivateArgs(ctx context.Context, flags []string, bundleDir, work string, stdin io.Reader) ([]string, error) {
	sameDir := func(path string) error {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		want, werr := filepath.EvalSymlinks(bundleDir)
		if err != nil || werr != nil || resolved != want {
			return fmt.Errorf("--bundle-dir %s is not %s, the directory of the bundle", path, bundleDir)
		}
		return nil
	}
	readStdin := func() (string, error) {
		path := filepath.Join(work, "input")
		if _, err := os.Stat(path); err == nil {
			return "", fmt.Errorf("only one import flag can read standard input")
		}
		// An interrupt cancels ctx; a read of a pipe whose writer stays open
		// would otherwise never return.
		type read struct {
			raw []byte
			err error
		}
		done := make(chan read, 1)
		go func() {
			raw, err := io.ReadAll(stdin)
			done <- read{raw, err}
		}()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case result := <-done:
			if result.err != nil {
				return "", result.err
			}
			return path, os.WriteFile(path, result.raw, 0o600)
		}
	}
	var args []string
	for i := 0; i < len(flags); i++ {
		flag := flags[i]
		switch {
		case flag == "--dry-run" || strings.HasPrefix(flag, "--dry-run="):
			return nil, fmt.Errorf("activate runs the import without --dry-run")
		case flag == "--txn-token-file" || strings.HasPrefix(flag, "--txn-token-file="):
			// The import's client exits the process when it cannot read the
			// file, which would leave the locks held; activate reads it first.
			return nil, fmt.Errorf("pass --txn-token-file before --, where activate reads it before taking its locks")
		case flag == "--namespace" || strings.HasPrefix(flag, "--namespace=") || (strings.HasPrefix(flag, "-n") && !strings.HasPrefix(flag, "--")):
			// The runtimes' namespace would silently replace it.
			return nil, fmt.Errorf("pass -n before --, where it names the namespace of the runtimes and the Agents")
		case kagentAPIFlag(flag) != "":
			// The last value wins, so it would replace the Orka API activate
			// checked against --context. The value may be a token: name only
			// the flag.
			return nil, fmt.Errorf("pass %s before --, where activate checks the Orka API against --context", kagentAPIFlag(flag))
		case flag == "--bundle-dir":
			if i+1 == len(flags) {
				return nil, fmt.Errorf("--bundle-dir needs a directory")
			}
			i++
			if err := sameDir(flags[i]); err != nil {
				return nil, err
			}
		case strings.HasPrefix(flag, "--bundle-dir="):
			if err := sameDir(strings.TrimPrefix(flag, "--bundle-dir=")); err != nil {
				return nil, err
			}
		case slices.Contains(kagentInputFlags, flag):
			if i+1 == len(flags) {
				return nil, fmt.Errorf("%s needs a file", flag)
			}
			i++
			file := flags[i]
			if file == "-" {
				var err error
				if file, err = readStdin(); err != nil {
					return nil, err
				}
			}
			args = append(args, flag, file)
		case kagentStdinFlag(flag) != "":
			file, err := readStdin()
			if err != nil {
				return nil, err
			}
			args = append(args, kagentStdinFlag(flag), file)
		default:
			args = append(args, flag)
		}
	}
	return args, nil
}

// kagentAPIFlag names the flag that flag sets when it selects the Orka API
// or the credentials the import sends it, and is empty otherwise.
func kagentAPIFlag(flag string) string {
	name, _, _ := strings.Cut(flag, "=")
	switch {
	case name == "--server" || name == "--token" || name == "--txn-token" || name == "--kubeconfig":
		return name
	case !strings.HasPrefix(flag, "--") && (strings.HasPrefix(flag, "-s") || strings.HasPrefix(flag, "-t")):
		return flag[:2]
	}
	return ""
}

// kagentInputFlags are the import flags that read a file, or standard input
// for -. Activate runs the import twice, so it reads standard input once.
var kagentInputFlags = []string{"-f", "--file", "--revision-egress", "--revision-sources"}

// kagentStdinFlag names the input flag that flag reads standard input with
// in one argument, -f- or <flag>=-, and is empty otherwise.
func kagentStdinFlag(flag string) string {
	if flag == "-f-" {
		return "-f"
	}
	if name, value, ok := strings.Cut(flag, "="); ok && value == "-" && slices.Contains(kagentInputFlags, name) {
		return name
	}
	return ""
}

// kagentReadsStdin reports whether the import flags read their input from
// standard input.
func kagentReadsStdin(flags []string) bool {
	for i, flag := range flags {
		if kagentStdinFlag(flag) != "" || (slices.Contains(kagentInputFlags, flag) && i+1 < len(flags) && flags[i+1] == "-") {
			return true
		}
	}
	return false
}

// kagentRuntimeKinds are the kinds a runtime's objects in its own namespace
// have, which retire deletes by label.
func kagentRuntimeKinds() []client.ObjectList {
	return []client.ObjectList{&appsv1.DeploymentList{}, &corev1.ServiceList{}, &corev1.PersistentVolumeClaimList{},
		&corev1.ServiceAccountList{}, &corev1.SecretList{}, &corev1.ConfigMapList{}}
}

func (k *kagentRuntimes) retire(ctx context.Context, dir string) error {
	bundle, _, err := readKagentBundle(dir)
	if err != nil {
		return err
	}
	if err := k.lock(ctx, bundle.Agent); err != nil {
		return err
	}
	if ref, err := k.agentRuntimeRef(ctx, bundle.Agent); err != nil || ref != bundle.Runtime {
		return fmt.Errorf("agent %s does not use %s yet; activate it first (%v)", bundle.Agent, bundle.Runtime, err)
	}
	// The relay may select a runtime this would delete.
	if err := k.requireRelay(ctx, dir, bundle); err != nil {
		return err
	}
	// A runtime's objects may outlive its registration when an earlier retire
	// timed out or was interrupted, so they name runtimes to retire too. Only
	// runtimes of this bundle's kagent Agent count: a same-named kagent Agent
	// of another namespace maps to the same Orka Agent.
	ofAgent := client.MatchingLabels{kagentAgentLabel: bundle.Agent}
	olds := map[string]bool{}
	var registered corev1alpha1.AgentRuntimeList
	if err := k.c.List(ctx, &registered, client.InNamespace(k.namespace), ofAgent); err != nil {
		return err
	}
	for _, item := range registered.Items {
		if item.Annotations[kagentSourceAnnotation] == kagentSource(bundle) {
			olds[item.Name] = true
		}
	}
	type search struct {
		list client.ObjectList
		opts []client.ListOption
	}
	var searches []search
	for _, list := range kagentRuntimeKinds() {
		searches = append(searches, search{list, []client.ListOption{client.InNamespace(k.namespace), ofAgent}})
	}
	// A retire interrupted before it deleted a v0 runtime's pod reader leaves
	// only that, in the kagent Agent's namespace, labeled with this one. Only
	// v0 runtimes have one, so a v1 retire reads nothing there.
	readerNamespace := ""
	if bundle.KagentVersion == kagentVersionV0 {
		readerNamespace = bundle.KagentNamespace
		readers := client.MatchingLabels{kagentAgentLabel: bundle.Agent, kagentRuntimeNamespaceLabel: k.namespace}
		for _, list := range []client.ObjectList{&rbacv1.RoleList{}, &rbacv1.RoleBindingList{}} {
			searches = append(searches, search{list, []client.ListOption{client.InNamespace(readerNamespace), readers}})
		}
	}
	for _, search := range searches {
		list := search.list
		if err := k.c.List(ctx, list, search.opts...); err != nil {
			return err
		}
		items, err := metaItems(list)
		if err != nil {
			return err
		}
		for _, item := range items {
			if name := item.GetLabels()[kagentRuntimeLabel]; name != "" && item.GetAnnotations()[kagentSourceAnnotation] == kagentSource(bundle) {
				olds[name] = true
			}
		}
	}
	delete(olds, bundle.Runtime)
	for _, old := range slices.Sorted(maps.Keys(olds)) {
		if err := k.retireRuntime(ctx, old, readerNamespace); err != nil {
			return err
		}
	}
	return nil
}

func metaItems(list client.ObjectList) ([]metav1.Object, error) {
	raw, err := json.Marshal(list)
	if err != nil {
		return nil, err
	}
	var decoded struct {
		Items []metav1.PartialObjectMetadata `json:"items"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	out := make([]metav1.Object, 0, len(decoded.Items))
	for i := range decoded.Items {
		out = append(out, &decoded.Items[i])
	}
	return out, nil
}

// retireRuntime deletes the registration old and then its objects, including
// a v0 runtime's pod reader in readerNamespace; a v1 runtime passes none.
func (k *kagentRuntimes) retireRuntime(ctx context.Context, old, readerNamespace string) error {
	registration := &corev1alpha1.AgentRuntime{}
	found, err := k.getOptional(ctx, old, registration)
	if err != nil {
		return err
	}
	if found {
		k.logf("Retiring %s; Orka drains it first.", old)
		if err := k.c.Delete(ctx, registration); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		err := k.waitFor(ctx, k.options.RetireTimeout, "AgentRuntime "+old+" to drain", func(ctx context.Context) (bool, error) {
			found, err := k.getOptional(ctx, old, &corev1alpha1.AgentRuntime{})
			return !found, err
		})
		if err != nil {
			return err
		}
	}
	// The objects serve the registration until it is gone.
	if found, err := k.getOptional(ctx, old, &corev1alpha1.AgentRuntime{}); err != nil || found {
		return fmt.Errorf("AgentRuntime %s is still registered (%v)", old, err)
	}
	ofRuntime := client.MatchingLabels{kagentRuntimeLabel: old}
	for _, obj := range []client.Object{&appsv1.Deployment{}, &corev1.PersistentVolumeClaim{}, &corev1.ServiceAccount{}, &corev1.Secret{}, &corev1.ConfigMap{}} {
		if err := k.c.DeleteAllOf(ctx, obj, client.InNamespace(k.namespace), ofRuntime); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	// Services do not support deleting a collection.
	var services corev1.ServiceList
	if err := k.c.List(ctx, &services, client.InNamespace(k.namespace), ofRuntime); err != nil {
		return err
	}
	for i := range services.Items {
		if err := k.c.Delete(ctx, &services.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	// A v0 runtime's pod reader lives in the kagent Agent's namespace.
	if readerNamespace == "" {
		return nil
	}
	reader := client.MatchingLabels{kagentRuntimeLabel: old, kagentRuntimeNamespaceLabel: k.namespace}
	var roles rbacv1.RoleList
	var bindings rbacv1.RoleBindingList
	if err := k.c.List(ctx, &roles, client.InNamespace(readerNamespace), reader); err != nil {
		return err
	}
	if err := k.c.List(ctx, &bindings, client.InNamespace(readerNamespace), reader); err != nil {
		return err
	}
	for i := range roles.Items {
		if err := k.c.Delete(ctx, &roles.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	for i := range bindings.Items {
		if err := k.c.Delete(ctx, &bindings.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
