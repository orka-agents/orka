package patchverification

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"

	kvalidation "k8s.io/apimachinery/pkg/util/validation"
)

// ApplicationRuntimeQualification is operator-owned qualification evidence,
// never a model plan field or runtime-name inference. Backend admission must
// recheck the exact cluster/node/RuntimeClass identities and configuration.
type ApplicationRuntimeQualification struct {
	Version               int    `json:"version"`
	Profile               string `json:"profile"`
	QualificationDigest   string `json:"qualificationDigest"`
	TargetClusterUID      string `json:"targetClusterUID"`
	NodeName              string `json:"nodeName"`
	NodeUID               string `json:"nodeUID"`
	RuntimeClassName      string `json:"runtimeClassName"`
	RuntimeClassUID       string `json:"runtimeClassUID"`
	RuntimeHandler        string `json:"runtimeHandler"`
	RuntimeBinaryDigest   string `json:"runtimeBinaryDigest"`
	RuntimeBaseSpecDigest string `json:"runtimeBaseSpecDigest"`
	RuntimeOptionsDigest  string `json:"runtimeOptionsDigest"`
	NativeSwapMax         uint64 `json:"nativeSwapMax"`
	NativeSwapSource      string `json:"nativeSwapSource"`
	AggregatePVCBytes     int64  `json:"aggregatePVCBytes"`
	NodeMemoryBytes       int64  `json:"nodeMemoryBytes"`
	AggregateLogBytes     int64  `json:"aggregateLogBytes"`
	StorageQuotaMode      string `json:"storageQuotaMode"`
	StorageDurability     string `json:"storageDurability"`
}

// Limits are the exact rendered role limits, not the qualification maxima.
type ApplicationRuntimeLimits struct {
	MemoryMax   uint64 `json:"memoryMax"`
	CPUQuota    uint64 `json:"cpuQuota"`
	CPUPeriod   uint64 `json:"cpuPeriod"`
	PIDsMax     uint64 `json:"pidsMax"`
	FileSizeMax uint64 `json:"fileSizeMax"`
}

type ApplicationRuntimeConfig struct {
	Version       int                             `json:"version"`
	Qualification ApplicationRuntimeQualification `json:"qualification"`
	Limits        ApplicationRuntimeLimits        `json:"limits"`
}

func ValidateApplicationRuntimeQualification(value ApplicationRuntimeQualification) error {
	if value.Version != ApplicationProtocolVersion || value.Profile != ApplicationRuntimeGVisorBoundedV1 ||
		!validManifestUTF8(reflect.ValueOf(value)) || !sha256Pattern.MatchString(value.QualificationDigest) ||
		!identifierPattern.MatchString(value.TargetClusterUID) || len(kvalidation.IsDNS1123Subdomain(value.NodeName)) != 0 ||
		value.NodeName == "" || !identifierPattern.MatchString(value.NodeUID) ||
		!applicationName(value.RuntimeClassName) || !identifierPattern.MatchString(value.RuntimeClassUID) ||
		!applicationName(value.RuntimeHandler) || !sha256Pattern.MatchString(value.RuntimeBinaryDigest) ||
		!sha256Pattern.MatchString(value.RuntimeBaseSpecDigest) || !sha256Pattern.MatchString(value.RuntimeOptionsDigest) || value.NativeSwapMax != 0 ||
		value.NativeSwapSource != "qualified-native-ancestor" ||
		value.AggregatePVCBytes != 6<<30 || value.NodeMemoryBytes != 16<<30 || value.AggregateLogBytes != 512<<20 ||
		value.StorageQuotaMode != "aggregate-only-no-per-pvc-guarantee" ||
		value.StorageDurability != "ephemeral-node-loss-fail-closed" {
		return errors.New("application runtime qualification does not describe the admitted bounded profile")
	}
	return nil
}

func ApplicationRuntimeQualificationDigest(value ApplicationRuntimeQualification) (string, error) {
	if err := ValidateApplicationRuntimeQualification(value); err != nil {
		return "", err
	}
	content, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return Digest(content), nil
}

func ValidateApplicationRuntimeLimits(limits ApplicationRuntimeLimits) error {
	if limits.MemoryMax == 0 || limits.MemoryMax > 2<<30 || limits.CPUQuota == 0 ||
		limits.CPUPeriod == 0 || limits.CPUPeriod > 1000000 || limits.CPUQuota > 2*limits.CPUPeriod ||
		limits.PIDsMax != 512 || (limits.FileSizeMax != 16<<20 && limits.FileSizeMax != 512<<20) {
		return errors.New("application role limits exceed the qualified runtime ceiling")
	}
	return nil
}

func ApplicationFileSizeLimit(role string) (uint64, error) {
	switch role {
	case "prepare", "build", "initialize":
		return 512 << 20, nil
	case "", "execute":
		return 16 << 20, nil
	default:
		return 0, errors.New("application file-size role is not admitted")
	}
}

func DecodeApplicationRuntimeConfig(reader io.Reader) (ApplicationRuntimeConfig, error) {
	var config ApplicationRuntimeConfig
	if err := decodePreparationJSON(reader, &config, 16<<10); err != nil {
		return config, err
	}
	if config.Version != ApplicationProtocolVersion {
		return config, ErrEvidence
	}
	if err := ValidateApplicationRuntimeQualification(config.Qualification); err != nil {
		return config, err
	}
	return config, ValidateApplicationRuntimeLimits(config.Limits)
}

func validateApplicationRuntimeBoundary(input ApplicationBoundaryInput, report ApplicationBoundaryReport) error {
	qualification := input.RuntimeQualification
	if qualification == nil {
		if input.RuntimeLimits != nil || report.RuntimeProfile != "" || report.RuntimeQualificationDigest != "" ||
			report.PIDLimitSource != "" || report.SwapLimitSource != "" || report.GuestPIDSoft != 0 || report.GuestPIDHard != 0 {
			return ErrBinding
		}
		return nil
	}
	digest, err := ApplicationRuntimeQualificationDigest(*qualification)
	if err != nil || input.RuntimeLimits == nil || ValidateApplicationRuntimeLimits(*input.RuntimeLimits) != nil ||
		qualification.TargetClusterUID != input.Target.ClusterUID ||
		report.RuntimeProfile != ApplicationRuntimeGVisorBoundedV1 || report.RuntimeQualificationDigest != digest ||
		report.PIDLimitSource != "guest-rlimit-nproc" || report.SwapLimitSource != qualification.NativeSwapSource ||
		report.GuestPIDSoft != 512 || report.GuestPIDHard != 512 || report.SwapMax != qualification.NativeSwapMax ||
		report.MemoryMax != input.RuntimeLimits.MemoryMax || report.PIDsMax != input.RuntimeLimits.PIDsMax ||
		report.CPUQuota != input.RuntimeLimits.CPUQuota || report.CPUPeriod != input.RuntimeLimits.CPUPeriod ||
		report.FileSizeSoft != input.RuntimeLimits.FileSizeMax || report.FileSizeHard != input.RuntimeLimits.FileSizeMax {
		return errors.New("application runtime evidence differs from its qualified mechanism or exact role limits")
	}
	return nil
}
