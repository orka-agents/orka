/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
)

const (
	// toolHealthCheckInterval is how often tools are re-checked.
	toolHealthCheckInterval = 5 * time.Minute

	// toolHealthCheckTimeout is the HTTP timeout for health checks.
	toolHealthCheckTimeout = 10 * time.Second
)

// ToolReconciler reconciles a Tool object
type ToolReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// HTTPClient is the HTTP client used for health checks. If nil, a default is used.
	HTTPClient *http.Client
}

// +kubebuilder:rbac:groups=core.mercan.ai,resources=tools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.mercan.ai,resources=tools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.mercan.ai,resources=tools/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

// Reconcile validates the Tool configuration, performs a health check on the HTTP endpoint,
// and updates the Tool's status accordingly.
func (r *ToolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Tool
	tool := &corev1alpha1.Tool{}
	if err := r.Get(ctx, req.NamespacedName, tool); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling Tool", "tool", tool.Name, "url", tool.Spec.HTTP.URL)

	// Validate the tool configuration
	if err := r.validateTool(ctx, tool); err != nil {
		logger.Error(err, "Tool validation failed")
		return r.updateStatus(ctx, tool, false, err.Error())
	}

	// Perform health check on the HTTP endpoint
	if err := r.healthCheck(ctx, tool); err != nil {
		logger.Info("Tool health check failed", "tool", tool.Name, "error", err.Error())
		return r.updateStatus(ctx, tool, false, err.Error())
	}

	// Tool is valid and reachable
	return r.updateStatus(ctx, tool, true, "")
}

// validateTool validates the Tool spec.
func (r *ToolReconciler) validateTool(ctx context.Context, tool *corev1alpha1.Tool) error {
	// Validate URL
	if tool.Spec.HTTP.URL == "" {
		return fmt.Errorf("http.url is required")
	}
	if _, err := url.ParseRequestURI(tool.Spec.HTTP.URL); err != nil {
		return fmt.Errorf("invalid http.url %q: %w", tool.Spec.HTTP.URL, err)
	}

	// Validate description
	if tool.Spec.Description == "" {
		return fmt.Errorf("description is required")
	}

	// Validate authSecretRef if set
	if tool.Spec.HTTP.AuthSecretRef != nil {
		secret := &corev1.Secret{}
		key := client.ObjectKey{Name: tool.Spec.HTTP.AuthSecretRef.Name, Namespace: tool.Namespace}
		if err := r.Get(ctx, key, secret); err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("referenced auth secret %q not found", tool.Spec.HTTP.AuthSecretRef.Name)
			}
			return fmt.Errorf("failed to get auth secret %q: %w", tool.Spec.HTTP.AuthSecretRef.Name, err)
		}
		if _, ok := secret.Data[tool.Spec.HTTP.AuthSecretRef.Key]; !ok {
			return fmt.Errorf("key %q not found in auth secret %q", tool.Spec.HTTP.AuthSecretRef.Key, tool.Spec.HTTP.AuthSecretRef.Name)
		}
	}

	// Validate authInject + authBodyKey combination
	if tool.Spec.HTTP.AuthInject == "body" && tool.Spec.HTTP.AuthBodyKey == "" {
		return fmt.Errorf("authBodyKey is required when authInject is 'body'")
	}

	return nil
}

// healthCheck performs an HTTP health check against the tool endpoint.
func (r *ToolReconciler) healthCheck(ctx context.Context, tool *corev1alpha1.Tool) error {
	httpClient := r.getHTTPClient()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, tool.Spec.HTTP.URL, nil)
	if err != nil {
		return fmt.Errorf("failed to create health check request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("endpoint unreachable: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	// Any response (even 4xx/5xx) means the endpoint is reachable.
	// We only mark unavailable if the connection itself fails.
	return nil
}

// getHTTPClient returns the HTTP client for health checks.
func (r *ToolReconciler) getHTTPClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return &http.Client{
		Timeout: toolHealthCheckTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
		},
	}
}

// updateStatus updates the Tool status and conditions.
func (r *ToolReconciler) updateStatus(ctx context.Context, tool *corev1alpha1.Tool, available bool, errMsg string) (ctrl.Result, error) {
	now := metav1.Now()

	tool.Status.Available = available
	tool.Status.LastCheck = &now
	tool.Status.Error = errMsg

	condition := metav1.Condition{
		Type:               "Available",
		LastTransitionTime: now,
		ObservedGeneration: tool.Generation,
	}

	if available {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "EndpointReachable"
		condition.Message = "Tool endpoint is reachable"
	} else {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "EndpointUnreachable"
		condition.Message = errMsg
	}

	meta.SetStatusCondition(&tool.Status.Conditions, condition)

	if err := r.Status().Update(ctx, tool); err != nil {
		return ctrl.Result{}, err
	}

	// Re-check periodically
	return ctrl.Result{RequeueAfter: toolHealthCheckInterval}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ToolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Tool{}).
		Named("tool").
		Complete(r)
}
