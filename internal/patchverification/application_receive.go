package patchverification

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"reflect"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

var errApplicationReceiverRoutePending = errors.New("application receiver route is not yet published")

type applicationReceiver struct {
	listener  *net.TCPListener
	input     ApplicationWorkerInput
	check     ApplicationTCPCheck
	kube      kubernetes.Interface
	subjectIP net.IP
	selfIP    string
	marker    []byte
	serviceIP string
	route     *ApplicationReceiverRoute
	capture   *applicationReceiverCapture
	stop      func()
}

func newApplicationReceiver(ctx context.Context, input ApplicationWorkerInput, check Check, kube kubernetes.Interface, subjectIP string, marker []byte) (*applicationReceiver, error) {
	tcp := check.Application.TCP
	ref := input.ReceiverService
	if tcp == nil || tcp.Mode != ApplicationTCPReceive || ref == nil || net.ParseIP(subjectIP) == nil ||
		len(marker) < 32 || len(marker) > 128 || Digest(marker) != input.MarkerDigest ||
		!applicationReceiverServiceIdentityValid(*ref, input.Worker) ||
		ref.Port != tcp.Port || len(ref.Selector) == 0 || len(ref.Selector) > 8 {
		return nil, ErrBinding
	}
	self, err := applicationLiveContainer(ctx, kube, input.Target, input.Worker,
		input.Manifest.Environment.Dependencies[ApplicationObserverImageKey], false)
	if err != nil || net.ParseIP(self.Status.PodIP) == nil {
		return nil, ErrBinding
	}
	address, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(self.Status.PodIP, strconv.Itoa(tcp.Port)))
	if err != nil {
		return nil, ErrEvidence
	}
	listener, err := net.ListenTCP("tcp", address)
	if err != nil {
		return nil, errors.New("application receiver could not bind its exact Pod listener")
	}
	receiver := &applicationReceiver{listener: listener, input: input, check: *tcp, kube: kube,
		subjectIP: net.ParseIP(subjectIP), selfIP: self.Status.PodIP, marker: marker}
	receiver.stop = applicationCloseOnCancel(ctx, listener)
	if err := receiver.waitForServiceRoute(ctx); err != nil {
		receiver.close()
		return nil, err
	}
	if err := receiver.verifyServiceDataPlane(ctx); err != nil {
		receiver.close()
		return nil, err
	}
	if err := receiver.verifyService(ctx); err != nil {
		receiver.close()
		return nil, err
	}
	if err := receiver.startCapture(ctx); err != nil {
		receiver.close()
		return nil, err
	}
	return receiver, nil
}

func (receiver *applicationReceiver) waitForServiceRoute(ctx context.Context) error {
	for {
		err := receiver.verifyService(ctx)
		if !errors.Is(err, errApplicationReceiverRoutePending) {
			return err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.New("application receiver Service routing did not become ready")
		case <-timer.C:
		}
	}
}

func (receiver *applicationReceiver) close() {
	if receiver.capture != nil {
		receiver.capture.cancel()
	}
	_ = receiver.listener.Close()
	if receiver.stop != nil {
		receiver.stop()
	}
	if receiver.capture != nil {
		<-receiver.capture.done
	}
}

func (receiver *applicationReceiver) verifyService(ctx context.Context) error {
	ref := receiver.input.ReceiverService
	service, err := receiver.kube.CoreV1().Services(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil || service == nil || service.Namespace != ref.Namespace || service.Name != ref.Name ||
		string(service.UID) != ref.UID || service.DeletionTimestamp != nil ||
		service.Spec.Type != corev1.ServiceTypeClusterIP || !reflect.DeepEqual(service.Spec.Selector, ref.Selector) ||
		net.ParseIP(service.Spec.ClusterIP) == nil || len(service.Spec.ExternalIPs) != 0 ||
		service.Spec.PublishNotReadyAddresses || len(service.Spec.Ports) != 1 {
		return ErrBinding
	}
	port := service.Spec.Ports[0]
	if int(port.Port) != ref.Port || port.Protocol != corev1.ProtocolTCP || port.TargetPort.Type != intstr.Int ||
		port.TargetPort.IntValue() != receiver.check.Port || port.NodePort != 0 ||
		receiver.serviceIP != "" && receiver.serviceIP != service.Spec.ClusterIP {
		return ErrBinding
	}
	content, published := service.Annotations[ApplicationReceiverRouteAnnotation(receiver.input.Worker.PodUID)]
	if !published && receiver.route == nil {
		return errApplicationReceiverRoutePending
	}
	route, err := DecodeApplicationReceiverRoute(content, *ref, receiver.input.Worker)
	if err != nil || receiver.route != nil && *receiver.route != route {
		return ErrBinding
	}
	if err := receiver.verifyEndpointSlice(ctx, route, port.Name); err != nil {
		return err
	}
	receiver.serviceIP, receiver.route = service.Spec.ClusterIP, &route
	return nil
}

func (receiver *applicationReceiver) verifyEndpointSlice(ctx context.Context, route ApplicationReceiverRoute, portName string) error {
	ref := receiver.input.ReceiverService
	endpointSlice, err := receiver.kube.DiscoveryV1().EndpointSlices(route.Namespace).Get(
		ctx, route.EndpointSliceName, metav1.GetOptions{})
	if err != nil || endpointSlice == nil || endpointSlice.Name != route.EndpointSliceName ||
		string(endpointSlice.UID) != route.EndpointSliceUID ||
		endpointSlice.ResourceVersion != route.EndpointSliceResourceVersion {
		return ErrBinding
	}
	if err := ValidateApplicationReceiverEndpointSlice(*ref, receiver.input.Worker,
		receiver.selfIP, portName, endpointSlice); err != nil {
		return err
	}
	pod, err := applicationLiveContainer(ctx, receiver.kube, receiver.input.Target, receiver.input.Worker,
		receiver.input.Manifest.Environment.Dependencies[ApplicationObserverImageKey], false)
	if err != nil || pod.Status.PodIP != receiver.selfIP || pod.Status.Phase != corev1.PodRunning {
		return ErrBinding
	}
	for key, value := range ref.Selector {
		if pod.Labels[key] != value {
			return ErrBinding
		}
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return nil
		}
	}
	return ErrBinding
}

// API Endpoint readiness alone is not data-plane readiness. A fresh private
// nonce proves the Service routes to this exact listener before actor stimuli.
func (receiver *applicationReceiver) verifyServiceDataPlane(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return ErrEvidence
	}
	clientDone := make(chan error, 1)
	clientExited := make(chan struct{})
	defer func() {
		cancel()
		<-clientExited
	}()
	go func() {
		defer close(clientExited)
		connection, err := (&net.Dialer{}).DialContext(ctx, "tcp",
			net.JoinHostPort(receiver.serviceIP, strconv.Itoa(receiver.check.Port)))
		if err != nil {
			clientDone <- ErrEvidence
			return
		}
		defer func() { _ = connection.Close() }()
		stop := applicationCloseOnCancel(ctx, connection)
		defer stop()
		deadline, _ := ctx.Deadline()
		_ = connection.SetDeadline(deadline)
		if _, err := connection.Write(nonce); err != nil {
			clientDone <- ErrEvidence
			return
		}
		if tcp, ok := connection.(*net.TCPConn); !ok || tcp.CloseWrite() != nil {
			clientDone <- ErrEvidence
			return
		}
		reply, err := io.ReadAll(io.LimitReader(connection, 33))
		if err != nil || !bytes.Equal(reply, nonce) {
			clientDone <- ErrEvidence
			return
		}
		clientDone <- nil
	}()
	deadline, _ := ctx.Deadline()
	if receiver.listener.SetDeadline(deadline) != nil {
		return ErrEvidence
	}
	matched := false
	for range 16 {
		connection, err := receiver.listener.AcceptTCP()
		if err != nil {
			break
		}
		_ = connection.SetDeadline(deadline)
		content, readErr := io.ReadAll(io.LimitReader(connection, 33))
		if readErr == nil && bytes.Equal(content, nonce) {
			_, err = connection.Write(nonce)
			matched = err == nil
		} else if readErr != nil || len(content) != 0 {
			_ = connection.Close()
			return errors.New("application receiver received unbound traffic before its stimulus")
		}
		_ = connection.Close()
		if matched {
			break
		}
	}
	if !matched {
		return errors.New("application receiver Service did not route the positive data-plane control")
	}
	select {
	case err := <-clientDone:
		return err
	case <-ctx.Done():
		return ErrEvidence
	}
}

func (receiver *applicationReceiver) readConnectionFrame(connection *net.TCPConn, subject bool) ([]byte, error) {
	if !subject {
		// Readiness probes close without data; their empty connections must
		// neither count as subject behavior nor block another accepted frame.
		content, err := io.ReadAll(io.LimitReader(connection, 1))
		if len(content) != 0 {
			return nil, ErrBinding
		}
		return content, err
	}
	content, err := applicationReadFrame(connection, receiver.check.Terminator, receiver.check.MaxBytes)
	if err == nil && len(receiver.check.Response) != 0 {
		var count int
		count, err = connection.Write(receiver.check.Response)
		if count != len(receiver.check.Response) {
			err = io.ErrShortWrite
		}
	}
	return content, err
}

func applicationReadFrame(reader io.Reader, terminator []byte, limit int) ([]byte, error) {
	if limit < 0 || limit > MaxApplicationTCPBytes || len(terminator) > 8 {
		return nil, ErrLimit
	}
	if len(terminator) == 0 {
		content, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
		if err != nil || len(content) > limit {
			return nil, ErrEvidence
		}
		return content, nil
	}
	content := make([]byte, 0, min(limit, 1024))
	next := [1]byte{}
	for len(content) < limit {
		n, err := reader.Read(next[:])
		if n != 0 {
			content = append(content, next[0])
			if bytes.HasSuffix(content, terminator) {
				return content, nil
			}
		}
		if err != nil {
			return nil, ErrEvidence
		}
		if n == 0 {
			return nil, io.ErrNoProgress
		}
	}
	return nil, ErrLimit
}
