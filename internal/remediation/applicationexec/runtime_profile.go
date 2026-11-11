package applicationexec

import (
	"encoding/json"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
)

const (
	runtimeQualificationAnnotation = "applicationexec.orka.ai/runtime-qualification"
	runtimeBinaryAnnotation        = "applicationexec.orka.ai/runtime-binary"
	runtimeBaseSpecAnnotation      = "applicationexec.orka.ai/runtime-base-spec"
	runtimeOptionsAnnotation       = "applicationexec.orka.ai/runtime-options"
)

func (b *Backend) verifyQualifiedRuntime(node *corev1.Node, runtimeClass *nodev1.RuntimeClass) error {
	qualification := b.config.RuntimeQualification
	if qualification == nil {
		return nil
	}
	if qualification.TargetClusterUID != b.config.TargetClusterUID ||
		node.Name != qualification.NodeName || string(node.UID) != qualification.NodeUID ||
		runtimeClass == nil || runtimeClass.Name != qualification.RuntimeClassName ||
		string(runtimeClass.UID) != qualification.RuntimeClassUID || runtimeClass.Handler != qualification.RuntimeHandler ||
		runtimeClass.Overhead != nil ||
		node.Annotations[runtimeQualificationAnnotation] != qualification.QualificationDigest ||
		node.Annotations[runtimeBinaryAnnotation] != qualification.RuntimeBinaryDigest ||
		node.Annotations[runtimeBaseSpecAnnotation] != qualification.RuntimeBaseSpecDigest ||
		node.Annotations[runtimeOptionsAnnotation] != qualification.RuntimeOptionsDigest ||
		runtimeClass.Annotations[runtimeQualificationAnnotation] != qualification.QualificationDigest {
		return ErrIsolation
	}
	return nil
}

func (b *Backend) runtimeConfig(resources corev1.ResourceRequirements, fileRole string) (*pv.ApplicationRuntimeConfig, error) {
	if b.config.RuntimeQualification == nil {
		return nil, nil
	}
	memory, cpu := resources.Limits.Memory().Value(), resources.Limits.Cpu().MilliValue()
	if memory <= 0 || memory > 2<<30 || cpu <= 0 || cpu > 2000 {
		return nil, contract.ErrInvalid
	}
	fileSize, err := pv.ApplicationFileSizeLimit(fileRole)
	if err != nil {
		return nil, contract.ErrInvalid
	}
	return &pv.ApplicationRuntimeConfig{Version: pv.ApplicationProtocolVersion, Qualification: *b.config.RuntimeQualification,
		Limits: pv.ApplicationRuntimeLimits{MemoryMax: uint64(memory), CPUQuota: uint64(cpu) * 100,
			CPUPeriod: 100000, PIDsMax: 512, FileSizeMax: fileSize}}, nil
}

func (b *Backend) boundaryRuntime(input *pv.ApplicationBoundaryInput) error {
	if b.config.RuntimeQualification == nil {
		return nil
	}
	config, err := b.runtimeConfig(roleResources(workerModeBoundary, false), input.FileSizeRole)
	if err != nil {
		return err
	}
	input.RuntimeQualification = &config.Qualification
	input.RuntimeLimits = &config.Limits
	return nil
}

func (b *Backend) runtimeInputFiles(resources corev1.ResourceRequirements, fileRole string) (map[string][]byte, error) {
	files := make(map[string][]byte)
	config, err := b.runtimeConfig(resources, fileRole)
	if err != nil || config == nil {
		return files, err
	}
	content, err := json.Marshal(config)
	if err != nil {
		return nil, contract.ErrInvalid
	}
	files["runtime.json"] = content
	return files, nil
}
