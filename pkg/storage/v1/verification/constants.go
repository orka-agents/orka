package verification

const (
	SchemaVersion = 1
	Original      = "original"
	Patched       = "patched"
	Reproduction  = "reproduction"
	Normal        = "normal"
	Offline       = "offline"
	LocalServices = "local-services"

	MaxManifestBytes    = 1 << 20
	MaxObservationBytes = 32 << 10
	MaxOutputBytes      = 64 << 10
	MaxProvenanceBytes  = 32 << 20
	MaxRunBlobBytes     = 128 << 20
	MaxFrozenFiles      = 128
	MaxServices         = 8
	MaxIncidents        = 256
)
