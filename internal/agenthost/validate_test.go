/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package agenthost

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const testImage = "registry.example/host@sha256:" + "3333333333333333333333333333333333333333333333333333333333333333"

func validTemplate() *corev1alpha1.AgentHostTemplate {
	return &corev1alpha1.AgentHostTemplate{
		Spec: corev1alpha1.AgentHostTemplateSpec{
			SecurityContext: corev1alpha1.AgentHostSecurityContext{RunAsUser: 1000, RunAsGroup: 1000},
			Containers: []corev1alpha1.AgentHostContainer{{
				Name: "host", Image: testImage,
				VolumeMounts: []corev1alpha1.AgentHostVolumeMount{{Name: "workspace", MountPath: "/workspace"}, {Name: "tmp", MountPath: "/tmp"}},
			}},
			Volumes:        []corev1alpha1.AgentHostScratchVolume{{Name: "tmp"}},
			Storage:        corev1alpha1.AgentHostStorage{Size: resource.MustParse("1Gi")},
			ListenPort:     7777,
			Access:         corev1alpha1.AgentHostAccessRelay,
			GitHubToken:    corev1alpha1.AgentHostCredentialRequired,
			IdleStatusFile: "idle.json",
		},
	}
}

func TestValidateTemplate(t *testing.T) {
	if err := ValidateTemplate(validTemplate()); err != nil {
		t.Fatalf("valid template rejected: %v", err)
	}
	withEgress := validTemplate()
	withEgress.Spec.AdditionalEgress = []corev1alpha1.AgentHostEgressRule{{CIDR: "10.0.0.0/8", Ports: []int32{5432}}}
	if err := ValidateTemplate(withEgress); err != nil {
		t.Fatalf("valid egress rejected: %v", err)
	}
	cases := map[string]func(*corev1alpha1.AgentHostTemplateSpec){
		"no host container":   func(s *corev1alpha1.AgentHostTemplateSpec) { s.Containers[0].Name = "daemon" },
		"two host containers": func(s *corev1alpha1.AgentHostTemplateSpec) { s.Containers = append(s.Containers, s.Containers[0]) },
		"unpinned image":      func(s *corev1alpha1.AgentHostTemplateSpec) { s.Containers[0].Image = "registry.example/host:latest" },
		"reserved container":  func(s *corev1alpha1.AgentHostTemplateSpec) { s.Containers[0].Name = "orka-host-agent" },
		"reserved env": func(s *corev1alpha1.AgentHostTemplateSpec) {
			s.Containers[0].Env = []corev1alpha1.AgentHostEnvVar{{Name: "ORKA_HOST_ID"}}
		},
		"undeclared mount":    func(s *corev1alpha1.AgentHostTemplateSpec) { s.Containers[0].VolumeMounts[1].Name = "cache" },
		"reserved volume":     func(s *corev1alpha1.AgentHostTemplateSpec) { s.Volumes[0].Name = "orka-status" },
		"host-agent port":     func(s *corev1alpha1.AgentHostTemplateSpec) { s.ListenPort = corev1alpha1.AgentHostStatusPort },
		"privileged port":     func(s *corev1alpha1.AgentHostTemplateSpec) { s.ListenPort = 80 },
		"relay without token": func(s *corev1alpha1.AgentHostTemplateSpec) { s.GitHubToken = corev1alpha1.AgentHostCredentialOptional },
		"root user":           func(s *corev1alpha1.AgentHostTemplateSpec) { s.SecurityContext.RunAsUser = 0 },
		"status file path":    func(s *corev1alpha1.AgentHostTemplateSpec) { s.IdleStatusFile = "../idle.json" },
		"status file dot":     func(s *corev1alpha1.AgentHostTemplateSpec) { s.IdleStatusFile = "." },
		"status file dot dot": func(s *corev1alpha1.AgentHostTemplateSpec) { s.IdleStatusFile = ".." },
		"empty storage":       func(s *corev1alpha1.AgentHostTemplateSpec) { s.Storage.Size = resource.MustParse("0") },
		"unknown access":      func(s *corev1alpha1.AgentHostTemplateSpec) { s.Access = "Tunnel" },
		"host as init": func(s *corev1alpha1.AgentHostTemplateSpec) {
			s.InitContainers = []corev1alpha1.AgentHostContainer{s.Containers[0]}
		},
		"suspend without status": func(s *corev1alpha1.AgentHostTemplateSpec) {
			s.IdleStatusFile = ""
			s.IdleSuspend = &corev1alpha1.AgentHostIdleSuspend{After: metav1.Duration{Duration: time.Minute}}
		},
		"mount at the connection token": func(s *corev1alpha1.AgentHostTemplateSpec) {
			s.Containers[0].VolumeMounts[1].MountPath = "/run/orka/connection-token"
		},
		"mount under the status directory": func(s *corev1alpha1.AgentHostTemplateSpec) {
			s.Containers[0].VolumeMounts[1].MountPath = "/var/run/orka/status/../status"
		},
		"two mounts at one path": func(s *corev1alpha1.AgentHostTemplateSpec) {
			s.Containers[0].VolumeMounts[1].MountPath = "/workspace/"
		},
		"resource claim": func(s *corev1alpha1.AgentHostTemplateSpec) {
			s.Containers[0].Resources.Claims = []corev1.ResourceClaim{{Name: "gpu"}}
		},
		"negative scratch size": func(s *corev1alpha1.AgentHostTemplateSpec) {
			limit := resource.MustParse("-1Mi")
			s.Volumes[0].SizeLimit = &limit
		},
		"egress to a non-CIDR": func(s *corev1alpha1.AgentHostTemplateSpec) {
			s.AdditionalEgress = []corev1alpha1.AgentHostEgressRule{{CIDR: "not-a-cidr", Ports: []int32{443}}}
		},
		"egress to port zero": func(s *corev1alpha1.AgentHostTemplateSpec) {
			s.AdditionalEgress = []corev1alpha1.AgentHostEgressRule{{CIDR: "10.0.0.0/8", Ports: []int32{0}}}
		},
		"empty runtime class": func(s *corev1alpha1.AgentHostTemplateSpec) {
			empty := ""
			s.RuntimeClassName = &empty
		},
		"malformed runtime class": func(s *corev1alpha1.AgentHostTemplateSpec) {
			name := "Not_A_Class"
			s.RuntimeClassName = &name
		},
		"negative request": func(s *corev1alpha1.AgentHostTemplateSpec) {
			s.Containers[0].Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("-1")}
		},
		"request above its limit": func(s *corev1alpha1.AgentHostTemplateSpec) {
			s.Containers[0].Resources.Requests = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}
			s.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
		},
	}
	for name, mutate := range cases {
		template := validTemplate()
		mutate(&template.Spec)
		if err := ValidateTemplate(template); err == nil {
			t.Errorf("%s: template accepted", name)
		}
	}
}

func TestParseIdleStatusIgnoresUnknownFieldsAndBoundsSize(t *testing.T) {
	status, err := ParseIdleStatus([]byte(`{"idle":true,"idleSince":"2026-10-17T21:29:40Z","extra":1}`))
	if err != nil || !status.Idle || status.IdleSince == nil {
		t.Fatalf("status = %+v, err = %v", status, err)
	}
	if _, err := ParseIdleStatus(make([]byte, maxStatusBytes+1)); err == nil {
		t.Fatal("oversized status accepted")
	}
}
