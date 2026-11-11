package patchverification

import (
	"fmt"
)

const (
	KubernetesPolicyVersion = "kubernetes-v4-cgroup-pids"
	ValidationTaskPrefix    = "patchverification.orka.ai/"
	ValidationRequestKey    = ValidationTaskPrefix + "request"
	ValidationSideKey       = ValidationTaskPrefix + "side"
	ValidationManifestKey   = ValidationTaskPrefix + "manifest"
	ValidationBackendKey    = ValidationTaskPrefix + "backend"
	KubernetesBackend       = "kubernetes"
	PodProtocolVersion      = 2
	MaxPodInputBytes        = 512 << 10
	MaxPodReportBytes       = 1 << 20
)

type PodInput struct {
	Version    int      `json:"version"`
	Manifest   Manifest `json:"manifest"`
	Binding    Binding  `json:"binding"`
	Side       string   `json:"side"`
	CheckID    string   `json:"checkID"`
	Archive    []byte   `json:"archive"`
	CanaryHost string   `json:"canaryHost,omitempty"`
	CanaryPort int      `json:"canaryPort,omitempty"`
}

type PodReport struct {
	Version  int               `json:"version"`
	TaskUID  string            `json:"taskUID"`
	PodUID   string            `json:"podUID"`
	Evidence ExecutionEvidence `json:"evidence"`
}

// ContainsCredentialMaterial rejects recognized credential encodings before
// captured bytes enter a transport such as Kubernetes Pod logs.
func ContainsCredentialMaterial(content []byte) bool {
	return sourceCredentials.Match(content)
}

func KubernetesSeccompProfile(platform, profile string) ([]byte, error) {
	return workerSeccomp(platform, profile)
}

func ValidatePodInput(input PodInput) (Check, error) {
	if input.Version != PodProtocolVersion || ValidateManifest(input.Manifest) != nil ||
		ValidateRunBinding(input.Manifest, input.Binding) != nil {
		return Check{}, fmt.Errorf("invalid validation pod protocol or binding")
	}
	if input.Manifest.Environment.Dependencies["orka.kubernetes.policy"] != KubernetesPolicyVersion {
		return Check{}, fmt.Errorf("unsupported kubernetes validation policy")
	}
	source := input.Manifest.Sources.Original
	if input.Side == Patched && input.Manifest.Action != ValidateReport {
		source = input.Manifest.Sources.Patched
	} else if input.Side != Original {
		return Check{}, fmt.Errorf("invalid validation source side")
	}
	if len(input.Archive) > MaxProvenanceBytes || Digest(input.Archive) != source.ArchiveDigest {
		return Check{}, fmt.Errorf("validation source archive does not match its frozen identity")
	}
	for _, check := range input.Manifest.Checks {
		if check.ID == input.CheckID {
			return check, nil
		}
	}
	return Check{}, fmt.Errorf("validation pod does not name a frozen check")
}
