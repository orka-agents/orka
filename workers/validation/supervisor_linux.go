//go:build linux

package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"golang.org/x/sys/unix"
)

const (
	guardPath       = "/runner/orka-validation-guard"
	firstRoleUID    = 20000
	setupTimeout    = 30 * time.Second
	shutdownTimeout = 2 * time.Second
	cleanupTimeout  = 5 * time.Second
	canaryTimeout   = 2 * time.Second
	supervisorCaps  = 1<<unix.CAP_CHOWN | 1<<unix.CAP_FOWNER | 1<<unix.CAP_KILL | 1<<unix.CAP_SETGID | 1<<unix.CAP_SETUID
)

func protectDirectory(name string, mode os.FileMode) error {
	info, err := os.Lstat(name)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("required validation mount is unavailable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	resolved, resolveErr := filepath.EvalSymlinks(name)
	if !ok || stat.Uid != 0 || stat.Gid != 0 || resolveErr != nil || resolved != name {
		return errors.New("validation mounts must be exact root-owned directories")
	}
	if info.Mode().Perm() != mode && os.Chmod(name, mode) != nil {
		return errors.New("validation mount permissions could not be restricted")
	}
	return nil
}

func verifySupervisorProcessPolicy() error {
	if os.Getuid() != 0 || os.Geteuid() != 0 || os.Getgid() != 0 || os.Getegid() != 0 {
		return errors.New("validation supervisor requires its dedicated root identity")
	}
	if os.Getpid() != 1 {
		return errors.New("validation supervisor must own its private container PID namespace")
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return errors.New("validation supervisor security settings are unavailable")
	}
	values := make(map[string]string)
	for line := range strings.SplitSeq(string(status), "\n") {
		if key, value, found := strings.Cut(line, ":"); found {
			values[key] = strings.TrimSpace(value)
		}
	}
	for _, key := range []string{"CapEff", "CapPrm", "CapBnd"} {
		value, err := strconv.ParseUint(values[key], 16, 64)
		if err != nil || value != supervisorCaps {
			return errors.New("validation supervisor capabilities do not match the bounded policy")
		}
	}
	for _, key := range []string{"CapInh", "CapAmb"} {
		value, err := strconv.ParseUint(values[key], 16, 64)
		if err != nil || value != 0 {
			return errors.New("validation supervisor cannot inherit or delegate capabilities")
		}
	}
	if values["NoNewPrivs"] != "1" || values["Seccomp"] != "2" {
		return errors.New("validation supervisor requires RuntimeDefault and no-new-privileges")
	}
	return nil
}

func supervisorPreflight() error {
	if err := verifySupervisorPolicies(verifySupervisorProcessPolicy, pv.CheckWorkerCgroupBounds); err != nil {
		return err
	}
	var root unix.Statfs_t
	if unix.Statfs("/", &root) != nil || root.Flags&unix.ST_RDONLY == 0 {
		return errors.New("validation supervisor requires a read-only image filesystem")
	}
	if _, err := os.Lstat("/var/run/secrets/kubernetes.io/serviceaccount/token"); !os.IsNotExist(err) {
		return errors.New("validation supervisor cannot mount Kubernetes credentials")
	}
	if unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0) != nil ||
		unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) != nil {
		return errors.New("validation observer protection or descendant reaping is unavailable")
	}
	pidfd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return errors.New("exact process signaling is unavailable")
	}
	err = signalExact(pidfd, 0)
	_ = unix.Close(pidfd)
	if err != nil {
		return err
	}
	for _, mount := range []struct {
		name string
		mode os.FileMode
	}{
		{"/input", 0700}, {"/runner", 0500}, {"/src", 0700},
		{"/checks", 0700}, {"/work", 0711}, {"/tmp", 0711},
	} {
		if err := protectDirectory(mount.name, mount.mode); err != nil {
			return err
		}
	}
	// /dev/shm is a separate, normally writable container mount, not covered by
	// readOnlyRootFilesystem. Do not leave shared writable storage between roles.
	if _, err := os.Lstat("/dev/shm"); err == nil {
		if err := protectDirectory("/dev/shm", 0700); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return errors.New("shared-memory mount could not be restricted")
	}
	info, err := os.Lstat(guardPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&022 != 0 || info.Mode().Perm()&0111 == 0 {
		return errors.New("trusted validation guard is unavailable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || stat.Gid != 0 {
		return errors.New("trusted validation guard is not root-owned")
	}
	inventory, err := processInventory()
	if err != nil {
		return err
	}
	for _, process := range inventory {
		for _, uid := range process.uids {
			if uid >= firstRoleUID && uid <= firstRoleUID+pv.MaxServices {
				return errors.New("validation role identities are already in use")
			}
		}
	}
	return nil
}

func verifySupervisorPolicies(processPolicy, cgroupPolicy func() error) error {
	if err := processPolicy(); err != nil {
		return err
	}
	return cgroupPolicy()
}

func verifyCanary(ctx context.Context, host string, port int, budget time.Duration) error {
	return probeCanary(ctx, host, port, budget, (&net.Dialer{}).DialContext)
}

func probeCanary(
	ctx context.Context, host string, port int, budget time.Duration,
	dial func(context.Context, string, string) (net.Conn, error),
) error {
	address, err := netip.ParseAddr(host)
	if err != nil || address.IsLoopback() || address.IsUnspecified() || address.IsMulticast() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.Zone() != "" ||
		port < 1 || port > 65535 {
		return errors.New("local-services requires a trusted numeric non-local canary endpoint")
	}
	probe, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	connection, err := dial(probe, "tcp", net.JoinHostPort(address.String(), strconv.Itoa(port)))
	if connection != nil {
		_ = connection.Close()
		return errors.New("local-services canary is reachable: network isolation is not enforced")
	}
	if ctx.Err() != nil {
		return errors.New("local-services network isolation probe was canceled")
	}
	var networkError net.Error
	if err == nil || !errors.As(err, &networkError) || !networkError.Timeout() {
		// Refusal and routing errors do not attest enforcement of a deny policy.
		return errors.New("local-services canary did not confirm enforced network isolation")
	}
	return nil
}

func validateSubjectExit(process *childProcess, check pv.Check) (bool, error) {
	if !process.executed || !process.waited || process.startedAt.IsZero() ||
		process.finishedAt.Before(process.startedAt) || !process.status.Exited() ||
		process.status.ExitStatus() < 0 || process.status.ExitStatus() >= 125 {
		return false, errors.New("check did not return a usable supervised exit status")
	}
	exit := process.status.ExitStatus()
	if exit == 77 {
		return true, errors.New("check returned a skipped exit status")
	}
	if exit != check.Healthy.ExitCode && exit != check.Failure.ExitCode {
		return false, errors.New("check returned an undeclared exit status")
	}
	return false, nil
}

func preopenedListener(port int) (*os.File, error) {
	return preopenedTCPListener(unix.SockaddrInet6{Port: port})
}

func preopenedTCPListener(address unix.SockaddrInet6) (*os.File, error) {
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
	if err != nil {
		return nil, errors.New("local fixture socket activation is unavailable")
	}
	if unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 0) != nil ||
		unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1) != nil ||
		unix.Bind(fd, &address) != nil || unix.Listen(fd, 32) != nil {
		_ = unix.Close(fd)
		return nil, errors.New("local fixture listener could not be prepared")
	}
	return os.NewFile(uintptr(fd), "fixture-listener"), nil
}

func prepareRole(uid int) (string, string, error) {
	directory := strconv.Itoa(uid)
	work, scratch := filepath.Join("/work", directory), filepath.Join("/tmp", directory)
	for _, root := range []string{work, scratch} {
		if os.Mkdir(root, 0700) != nil || os.Chown(root, uid, uid) != nil || os.Chmod(root, 0700) != nil {
			return "", "", errors.New("private validation role directories could not be prepared")
		}
	}
	return work, scratch, nil
}

func startSandbox(
	manager *processManager, input pv.PodInput, command []string, uid int,
	service *pv.Service, stdin string, activatedListener *os.File,
) (*childProcess, error) {
	if activatedListener != nil && service == nil {
		return nil, errors.New("an activated listener requires an accept-only server role")
	}
	_, scratch, err := prepareRole(uid)
	if err != nil {
		return nil, err
	}
	fixture := service != nil
	environment, err := childEnvironment(input.Manifest.Environment.Variables, scratch, fixture)
	if err != nil {
		return nil, err
	}
	spec := processSpec{environment: environment, directory: "/", stdin: stdin, uid: uid, fixture: fixture}
	role, port := "subject", 0
	if fixture {
		role, port, spec.readyOutput = "fixture", service.Port, service.ReadyOutput
		spec.listener = activatedListener
		if spec.listener == nil {
			spec.listener, err = preopenedListener(port)
			if err != nil {
				return nil, err
			}
			defer func() { _ = spec.listener.Close() }()
		}
	}
	spec.command = append([]string{guardPath, strconv.Itoa(uid), input.Manifest.Environment.Profile,
		role, strconv.Itoa(port), "--"}, command...)
	return manager.start(spec)
}

func startHTTPServer(manager *processManager, input pv.PodInput, check pv.Check) (*childProcess, int, error) {
	listener, err := preopenedTCPListener(unix.SockaddrInet6{Addr: [16]byte{15: 1}})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = listener.Close() }()
	address, err := unix.Getsockname(int(listener.Fd()))
	if err != nil {
		return nil, 0, errors.New("HTTP subject listener identity is unavailable")
	}
	inet, ok := address.(*unix.SockaddrInet6)
	if !ok || inet.Addr != [16]byte{15: 1} || inet.Port < 1024 || inet.Port > 65535 {
		return nil, 0, errors.New("HTTP subject listener is not an assigned unprivileged loopback endpoint")
	}
	service := &pv.Service{Port: inet.Port}
	server, err := startSandbox(manager, input, pv.CheckExecutable(check), firstRoleUID, service, "", listener)
	return server, inet.Port, err
}

func runHTTPCheck(
	ctx context.Context, manager *processManager, input pv.PodInput, check pv.Check, evidence *pv.ExecutionEvidence,
) (server *childProcess, resultErr error) {
	running, cancel := context.WithTimeout(ctx, time.Duration(check.TimeoutSeconds)*time.Second)
	defer cancel()
	observation := &evidence.Observation
	defer func() {
		observation.TimedOut = errors.Is(running.Err(), context.DeadlineExceeded)
		if resultErr != nil {
			observation.HTTPCompleted = false
		}
	}()
	var port int
	server, port, resultErr = startHTTPServer(manager, input, check)
	if resultErr != nil {
		return server, resultErr
	}
	if err := manager.waitGuard(running, server); err != nil {
		return server, err
	}
	return server, manager.runHTTPObservation(running, server, check, port, evidence)
}

func runBoundInput(ctx context.Context, input pv.PodInput, check pv.Check, report *pv.PodReport) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	evidence := &report.Evidence
	fail := func(err error) {
		if err == nil {
			return
		}
		if evidence.Observation.SetupError == "" {
			evidence.Observation.SetupError = err.Error()
		}
		evidence.Observation.HTTPCompleted = false
	}
	if os.Getenv("ORKA_VALIDATION_TASK_UID") != report.TaskUID || !uidPattern.MatchString(report.PodUID) {
		fail(errors.New("validation task or pod identity is missing or mismatched"))
		return
	}
	if input.Manifest.Environment.Platform != "linux/"+runtime.GOARCH ||
		input.Manifest.Environment.Dependencies["orka.kubernetes.policy"] != pv.KubernetesPolicyVersion {
		fail(errors.New("validation platform or frozen Kubernetes policy does not match the worker"))
		return
	}
	if len(pv.MissingRequirements(input.Manifest.Environment)) != 0 {
		fail(errors.New("a declared validation environment requirement is unavailable"))
		return
	}
	if err := supervisorPreflight(); err != nil {
		fail(err)
		return
	}
	setup, cancelSetup := context.WithTimeout(ctx, setupTimeout)
	defer cancelSetup()
	if err := pv.StagePodInput(setup, input, "/src", "/checks"); err != nil {
		fail(err)
		return
	}
	if input.Manifest.Environment.Profile == pv.LocalServices {
		if err := verifyCanary(setup, input.CanaryHost, input.CanaryPort, canaryTimeout); err != nil {
			fail(err)
			return
		}
	}
	manager := newProcessManager()
	services := make(map[string]*childProcess)
	var subject *childProcess
	var httpServer *childProcess
	defer func() {
		fail(manager.completeEvidence(ctx, evidence, subject, services))
		if httpServer != nil {
			fail(captureHTTPDiagnostics(evidence, httpServer))
		}
	}()
	if check.HTTP != nil {
		var err error
		httpServer, err = runHTTPCheck(ctx, manager, input, check, evidence)
		fail(err)
		return
	}
	for index, service := range input.Manifest.Environment.Services {
		process, err := startSandbox(manager, input, service.Command, firstRoleUID+1+index, &service, "", nil)
		if err != nil {
			fail(err)
			return
		}
		services[service.ID] = process
		if err := manager.waitGuard(setup, process); err != nil {
			fail(err)
			return
		}
		if err := manager.waitReady(setup, process); err != nil {
			fail(err)
			return
		}
	}
	if manager.poll() != nil || !manager.fixturesAlive() || setup.Err() != nil {
		fail(errors.New("validation setup did not remain healthy"))
		return
	}
	running, cancelRunning := context.WithTimeout(ctx, time.Duration(check.TimeoutSeconds)*time.Second)
	defer cancelRunning()
	var err error
	subject, err = startSandbox(manager, input, check.Command, firstRoleUID, nil, check.Stdin, nil)
	if err == nil {
		err = manager.waitGuard(running, subject)
	}
	if err == nil {
		err = manager.waitSubject(running, subject)
	}
	evidence.Observation.TimedOut = errors.Is(running.Err(), context.DeadlineExceeded)
	if err != nil {
		fail(err)
		return
	}
	evidence.Observation.Skipped, err = validateSubjectExit(subject, check)
	if err != nil {
		fail(err)
		return
	}
	if remaining, err := manager.hasDescendants(subject.uid); err != nil || remaining {
		fail(errors.New("check left descendants running or their cleanup could not be observed"))
		return
	}
	if err := manager.stopFixtures(ctx); err != nil {
		fail(err)
		return
	}
	if ctx.Err() != nil {
		fail(errors.New("validation was canceled before evidence collection completed"))
	}
}

func (manager *processManager) completeEvidence(
	ctx context.Context, evidence *pv.ExecutionEvidence, subject *childProcess, services map[string]*childProcess,
) (resultErr error) {
	recordFirstError := func(err error) {
		if resultErr == nil {
			resultErr = err
		}
	}
	cleanup, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	recordFirstError(manager.cleanup(cleanup))
	cancelCleanup()
	drain, cancelDrain := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	recordFirstError(manager.drain(drain))
	cancelDrain()
	if subject != nil {
		observation := &evidence.Observation
		observation.Executed = subject.executed
		if subject.executed {
			observation.StartedAt = subject.startedAt
		}
		if subject.waited {
			observation.FinishedAt = subject.finishedAt
			if subject.status.Exited() {
				observation.ExitCode = new(subject.status.ExitStatus())
			}
		}
		stdout, stderr := captureEvidence(evidence, subject.stdout), captureEvidence(evidence, subject.stderr)
		observation.StdoutDigest, observation.StdoutBytes = stdout.Digest, stdout.Bytes
		observation.StderrDigest, observation.StderrBytes = stderr.Digest, stderr.Bytes
	}
	if len(services) != 0 {
		evidence.Observation.ServiceOutputs = make(map[string]pv.CapturedOutput)
	}
	for id, process := range services {
		evidence.Observation.ServiceOutputs[id] = captureEvidence(evidence, process.stdout)
		diagnostics, truncated := process.stderr.snapshot()
		evidence.Observation.OutputTruncated = evidence.Observation.OutputTruncated || truncated
		if len(diagnostics) != 0 || truncated {
			recordFirstError(errors.New("local fixture wrote unexpected diagnostic output"))
		}
	}
	if evidence.Observation.OutputTruncated {
		recordFirstError(errors.New("execution output exceeded the capture limit"))
	}
	if ctx.Err() != nil {
		recordFirstError(errors.New("validation was canceled before evidence collection completed"))
	}
	return resultErr
}
