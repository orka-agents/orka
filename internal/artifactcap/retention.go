package artifactcap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// DefaultRoot is the controller PVC subdirectory used by ACP artifact transport.
	DefaultRoot                   = "/data/acp-artifacts"
	defaultRetentionSweepInterval = time.Minute
	defaultRetentionLockTimeout   = 5 * time.Second
	retirementRecordMaxBytes      = 16 << 10
	reservationRecordMaxBytes     = 64 << 10
	operationRecordMaxBytes       = 64 << 10
)

// MinimumRetirementGrace keeps a retired identity blocked until every
// capability that could have been issued before retirement is expired.
const MinimumRetirementGrace = MaxCapabilityTTL + MaxClockSkew

// IdentityRetirer retires immutable artifact identities after their owning
// attempt/publication state can no longer require transport or replay.
type IdentityRetirer interface {
	Retire(context.Context, ...Identity) error
}

// CapabilityReservationRecorder persists a digest reservation before a
// capability is returned to a caller. Reservations keep cross-identity objects
// live until the capability expires or its identity is retired.
type CapabilityReservationRecorder interface {
	Reserve(context.Context, OperationRequest, time.Time) error
}

// CollectorConfig configures reference-aware artifact retention.
type CollectorConfig struct {
	Root          string
	SweepInterval time.Duration
	LockTimeout   time.Duration
	Now           func() time.Time
}

// Collector reclaims artifact objects and replay records only after the
// controller explicitly retires their immutable Task/publication identities.
// The artifact store and this retention format ship together in the ACP v2
// hard cutover, so there is no supported pre-retention ledger to backfill.
type Collector struct {
	root          string
	objectsDir    string
	metadataDir   string
	operationsDir string
	retirements   *retirementStore
	reservations  *reservationStore
	sweepInterval time.Duration
	lockTimeout   time.Duration
	now           func() time.Time
}

type retirementStore struct {
	dir string
}

type reservationStore struct {
	dir string
}

type identityRetirement struct {
	Identity   Identity  `json:"identity"`
	RetiredAt  time.Time `json:"retiredAt"`
	PurgeAfter time.Time `json:"purgeAfter"`
}

type operationFile struct {
	path   string
	record OperationRecord
}

type capabilityReservation struct {
	Request   OperationRequest `json:"request"`
	ExpiresAt time.Time        `json:"expiresAt"`
}

type reservationFile struct {
	path   string
	record capabilityReservation
}

func NewCollector(config CollectorConfig) (*Collector, error) {
	if config.Root == "" || !filepath.IsAbs(config.Root) {
		return nil, fmt.Errorf("%w: artifact root must be absolute", ErrUnsafePath)
	}
	root := filepath.Clean(config.Root)
	objectsDir := filepath.Join(root, "objects")
	metadataDir := filepath.Join(root, "metadata")
	operationsDir := filepath.Join(root, "operations")
	retirementsDir := filepath.Join(root, "retirements")
	reservationsDir := filepath.Join(root, "reservations")
	for _, dir := range []string{root, objectsDir, metadataDir, filepath.Join(root, "tmp"), operationsDir, retirementsDir, reservationsDir} {
		if err := ensurePrivateDirectory(dir); err != nil {
			return nil, err
		}
	}
	sweepInterval := config.SweepInterval
	if sweepInterval <= 0 {
		sweepInterval = defaultRetentionSweepInterval
	}
	lockTimeout := config.LockTimeout
	if lockTimeout <= 0 {
		lockTimeout = defaultRetentionLockTimeout
	}
	now := config.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Collector{
		root: root, objectsDir: objectsDir, metadataDir: metadataDir, operationsDir: operationsDir,
		retirements: &retirementStore{dir: retirementsDir}, reservations: &reservationStore{dir: reservationsDir},
		sweepInterval: sweepInterval, lockTimeout: lockTimeout, now: now,
	}, nil
}

// NeedLeaderElection keeps periodic sweeping on the controller leader. The
// filesystem lock still protects transport during rolling overlap.
func (*Collector) NeedLeaderElection() bool { return true }

// Start runs startup and periodic recovery sweeps. A direct Retire call still
// performs synchronous reclamation; the loop bounds short-lived tombstones and
// resumes cleanup after crashes.
func (c *Collector) Start(ctx context.Context) error {
	logger := logf.FromContext(ctx).WithName("acp-artifact-retention")
	if err := c.Sweep(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error(err, "initial artifact retention sweep failed")
	}
	ticker := time.NewTicker(c.sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := c.Sweep(ctx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error(err, "artifact retention sweep failed")
			}
		}
	}
}

// Reserve persists one exact capability reference before the capability is
// returned. The exclusive lock serializes reservation publication with
// reclamation; transport operations use the fair shared side of the same gate.
func (c *Collector) Reserve(ctx context.Context, request OperationRequest, expiresAt time.Time) error {
	if err := request.Validate(); err != nil {
		return err
	}
	now := c.now().UTC()
	expiresAt = expiresAt.UTC()
	if !expiresAt.After(now) || expiresAt.After(now.Add(MaxCapabilityTTL+MaxClockSkew)) {
		return fmt.Errorf("%w: capability reservation expiry is invalid", ErrInvalidRequest)
	}
	lockCtx, cancel := context.WithTimeout(ctx, c.lockTimeout)
	release, err := acquireArtifactRootLock(lockCtx, c.root, true)
	cancel()
	if err != nil {
		return err
	}
	defer func() { _ = release() }()
	retired, err := c.retirements.isRetired(ctx, request.Identity)
	if err != nil {
		return err
	}
	if retired {
		return ErrUnauthorized
	}
	if request.Operation == OperationDownload {
		if err := c.verifyReservedDownload(request); err != nil {
			return err
		}
	}
	return c.reservations.record(ctx, capabilityReservation{Request: request, ExpiresAt: expiresAt})
}

func (c *Collector) verifyReservedDownload(request OperationRequest) error {
	store := &FileObjectStore{root: c.root, objectsDir: c.objectsDir, metadataDir: c.metadataDir}
	artifact, err := store.readMetadata(request.ObjectDigest)
	if err != nil {
		return err
	}
	if artifact.SizeBytes != request.ContentLength || !constantStringEqual(artifact.MediaType, request.MediaType) {
		return ErrCorrupt
	}
	hexDigest, err := DigestHex(request.ObjectDigest)
	if err != nil {
		return err
	}
	file, err := openFileNoFollow(filepath.Join(c.objectsDir, hexDigest), os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	defer file.Close() //nolint:errcheck
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != request.ContentLength {
		return ErrCorrupt
	}
	return nil
}

// Retire persistently blocks the supplied immutable identities before
// reclaiming their unshared objects and replay records. Callers must establish
// that no attempt, publication, external effect, or outbox recovery can need
// the identities again.
func (c *Collector) Retire(ctx context.Context, identities ...Identity) error {
	if len(identities) == 0 {
		return fmt.Errorf("%w: at least one artifact identity is required", ErrInvalidRequest)
	}
	unique := make(map[string]Identity, len(identities))
	for _, identity := range identities {
		if err := identity.Validate(); err != nil {
			return err
		}
		key := identityStorageKey(identity)
		unique[key] = identity
	}
	keys := make([]string, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	lockCtx, cancel := context.WithTimeout(ctx, c.lockTimeout)
	release, err := acquireArtifactRootLock(lockCtx, c.root, true)
	cancel()
	if err != nil {
		return err
	}
	defer func() { _ = release() }()

	retiredAt := c.now().UTC()
	for _, key := range keys {
		if err := c.retirements.record(ctx, unique[key], retiredAt); err != nil {
			return err
		}
	}
	// Keep objects and replay records through the maximum capability lifetime.
	// This gives every capability issued to another live identity time to start
	// and persist its own replay reference before reclamation.
	return nil
}

// Sweep resumes crash-interrupted reclamation and prunes retirement tombstones
// only after the maximum capability lifetime has elapsed.
func (c *Collector) Sweep(ctx context.Context) error {
	lockCtx, cancel := context.WithTimeout(ctx, c.lockTimeout)
	release, err := acquireArtifactRootLock(lockCtx, c.root, true)
	cancel()
	if err != nil {
		return err
	}
	defer func() { _ = release() }()
	return c.sweepLocked(ctx, c.now().UTC())
}

func (c *Collector) sweepLocked(ctx context.Context, now time.Time) error {
	retirements, err := c.retirements.list(ctx)
	if err != nil {
		return err
	}
	operations, err := c.listOperations(ctx)
	if err != nil {
		return err
	}
	reservations, err := c.reservations.list(ctx)
	if err != nil {
		return err
	}
	reservedDigests, err := c.pruneCapabilityReservations(ctx, now, retirements, reservations)
	if err != nil {
		return err
	}
	liveDigests, candidateDigests, retiredOperations, err := classifyRetiredOperations(ctx, now, retirements, operations)
	if err != nil {
		return err
	}
	allLiveDigests := make(map[string]struct{}, len(liveDigests)+len(reservedDigests))
	for digest := range liveDigests {
		allLiveDigests[digest] = struct{}{}
	}
	for digest := range reservedDigests {
		allLiveDigests[digest] = struct{}{}
	}
	removableOperations, blockedRetirements := selectRemovableRetiredOperations(retiredOperations, liveDigests, reservedDigests)
	if err := c.removeRetiredArtifacts(ctx, allLiveDigests, candidateDigests); err != nil {
		return err
	}
	if err := c.removeRetiredOperationRecords(ctx, removableOperations); err != nil {
		return err
	}
	return c.removeExpiredRetirements(ctx, now, retirements, blockedRetirements)
}

func classifyRetiredOperations(
	ctx context.Context,
	now time.Time,
	retirements map[string]identityRetirement,
	operations []operationFile,
) (map[string]struct{}, map[string]struct{}, []operationFile, error) {
	liveDigests := make(map[string]struct{}, len(operations))
	candidateDigests := make(map[string]struct{})
	retiredOperations := make([]operationFile, 0)
	for _, operation := range operations {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		retirement, retired := retirements[identityStorageKey(operation.record.Request.Identity)]
		if retired && !retirement.PurgeAfter.After(now) {
			candidateDigests[operation.record.Request.ObjectDigest] = struct{}{}
			retiredOperations = append(retiredOperations, operation)
			continue
		}
		liveDigests[operation.record.Request.ObjectDigest] = struct{}{}
	}
	return liveDigests, candidateDigests, retiredOperations, nil
}

func (c *Collector) pruneCapabilityReservations(
	ctx context.Context,
	now time.Time,
	retirements map[string]identityRetirement,
	reservations []reservationFile,
) (map[string]struct{}, error) {
	live := make(map[string]struct{}, len(reservations))
	removed := false
	for _, reservation := range reservations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		_, retired := retirements[identityStorageKey(reservation.record.Request.Identity)]
		if retired || !reservation.record.ExpiresAt.After(now) {
			deleted, err := removeManagedRegularFile(reservation.path)
			if err != nil {
				return nil, fmt.Errorf("remove expired artifact capability reservation: %w", err)
			}
			removed = removed || deleted
			continue
		}
		live[reservation.record.Request.ObjectDigest] = struct{}{}
	}
	if removed {
		if err := syncDirectory(c.reservations.dir); err != nil {
			return nil, fmt.Errorf("sync artifact capability reservations: %w", err)
		}
	}
	return live, nil
}

func selectRemovableRetiredOperations(
	operations []operationFile,
	liveOperationDigests map[string]struct{},
	reservedDigests map[string]struct{},
) ([]operationFile, map[string]struct{}) {
	removable := make([]operationFile, 0, len(operations))
	blocked := make(map[string]struct{})
	reservationAnchors := make(map[string]struct{})
	for _, operation := range operations {
		digest := operation.record.Request.ObjectDigest
		_, liveOperation := liveOperationDigests[digest]
		_, reserved := reservedDigests[digest]
		if reserved && !liveOperation {
			if _, anchored := reservationAnchors[digest]; !anchored {
				reservationAnchors[digest] = struct{}{}
				blocked[identityStorageKey(operation.record.Request.Identity)] = struct{}{}
				continue
			}
		}
		removable = append(removable, operation)
	}
	return removable, blocked
}

func (c *Collector) removeRetiredArtifacts(ctx context.Context, liveDigests, candidateDigests map[string]struct{}) error {
	removedMetadata := false
	removedObjects := false
	for _, digest := range sortedSet(candidateDigests) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, retained := liveDigests[digest]; retained {
			continue
		}
		hexDigest, err := DigestHex(digest)
		if err != nil {
			return ErrCorrupt
		}
		removed, err := removeManagedRegularFile(filepath.Join(c.metadataDir, hexDigest+".json"))
		if err != nil {
			return fmt.Errorf("remove retired artifact metadata: %w", err)
		}
		removedMetadata = removedMetadata || removed
		removed, err = removeManagedRegularFile(filepath.Join(c.objectsDir, hexDigest))
		if err != nil {
			return fmt.Errorf("remove retired artifact object: %w", err)
		}
		removedObjects = removedObjects || removed
	}
	// Persist object/metadata removal before deleting the replay records that
	// identify crash-recovery candidates.
	if removedMetadata {
		if err := syncDirectory(c.metadataDir); err != nil {
			return fmt.Errorf("sync retired artifact metadata: %w", err)
		}
	}
	if removedObjects {
		if err := syncDirectory(c.objectsDir); err != nil {
			return fmt.Errorf("sync retired artifact objects: %w", err)
		}
	}
	return nil
}

func (c *Collector) removeRetiredOperationRecords(ctx context.Context, operations []operationFile) error {
	removed := false
	for _, operation := range operations {
		if err := ctx.Err(); err != nil {
			return err
		}
		deleted, err := removeManagedRegularFile(operation.path)
		if err != nil {
			return fmt.Errorf("remove retired artifact replay record: %w", err)
		}
		removed = removed || deleted
	}
	if removed {
		if err := syncDirectory(c.operationsDir); err != nil {
			return fmt.Errorf("sync retired artifact replay records: %w", err)
		}
	}
	return nil
}

func (c *Collector) removeExpiredRetirements(
	ctx context.Context,
	now time.Time,
	retirements map[string]identityRetirement,
	blocked map[string]struct{},
) error {
	removed := false
	for key, retirement := range retirements {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, keep := blocked[key]; keep {
			continue
		}
		if retirement.PurgeAfter.After(now) {
			continue
		}
		deleted, err := removeManagedRegularFile(c.retirements.pathForKey(key))
		if err != nil {
			return fmt.Errorf("remove expired artifact retirement record: %w", err)
		}
		removed = removed || deleted
	}
	if removed {
		if err := syncDirectory(c.retirements.dir); err != nil {
			return fmt.Errorf("sync artifact retirement records: %w", err)
		}
	}
	return nil
}

func (c *Collector) listOperations(ctx context.Context) ([]operationFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensurePrivateDirectory(c.operationsDir); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(c.operationsDir)
	if err != nil {
		return nil, fmt.Errorf("list artifact replay records: %w", err)
	}
	operations := make([]operationFile, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".operation-") && strings.HasSuffix(name, ".tmp") {
			if _, err := removeManagedRegularFile(filepath.Join(c.operationsDir, name)); err != nil {
				return nil, fmt.Errorf("remove stale artifact replay temp record: %w", err)
			}
			continue
		}
		if entry.IsDir() || len(name) != sha256.Size*2+len(".json") || !strings.HasSuffix(name, ".json") {
			return nil, ErrCorrupt
		}
		if _, err := hex.DecodeString(strings.TrimSuffix(name, ".json")); err != nil {
			return nil, ErrCorrupt
		}
		path := filepath.Join(c.operationsDir, name)
		record, err := readOperationRecordFile(path)
		if err != nil || operationRecordName(record.OperationID) != name {
			return nil, ErrCorrupt
		}
		operations = append(operations, operationFile{path: path, record: record})
	}
	return operations, nil
}

func (s *reservationStore) record(ctx context.Context, record capabilityReservation) error {
	if err := validateCapabilityReservation(record); err != nil {
		return err
	}
	if err := ensurePrivateDirectory(s.dir); err != nil {
		return err
	}
	path := s.pathForOperation(record.Request.OperationID)
	if existing, err := s.get(ctx, path); err == nil {
		if existing.Request != record.Request {
			return ErrOperationConflict
		}
		if !record.ExpiresAt.After(existing.ExpiresAt) {
			return nil
		}
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(s.dir, ".reservation-*.tmp")
	if err != nil {
		return fmt.Errorf("create artifact reservation temp record: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath) //nolint:errcheck
	if err = temp.Chmod(0o600); err == nil {
		_, err = temp.Write(data)
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("persist artifact reservation record: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("commit artifact reservation record: %w", err)
	}
	return syncDirectory(s.dir)
}

func (s *reservationStore) list(ctx context.Context) ([]reservationFile, error) {
	if err := ensurePrivateDirectory(s.dir); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("list artifact capability reservations: %w", err)
	}
	result := make([]reservationFile, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".reservation-") && strings.HasSuffix(name, ".tmp") {
			if _, err := removeManagedRegularFile(filepath.Join(s.dir, name)); err != nil {
				return nil, err
			}
			continue
		}
		if entry.IsDir() || len(name) != sha256.Size*2+len(".json") || !strings.HasSuffix(name, ".json") {
			return nil, ErrCorrupt
		}
		if _, err := hex.DecodeString(strings.TrimSuffix(name, ".json")); err != nil {
			return nil, ErrCorrupt
		}
		path := filepath.Join(s.dir, name)
		record, err := s.get(ctx, path)
		if err != nil || operationRecordName(record.Request.OperationID) != name {
			return nil, ErrCorrupt
		}
		result = append(result, reservationFile{path: path, record: record})
	}
	return result, nil
}

func (s *reservationStore) get(ctx context.Context, path string) (capabilityReservation, error) {
	if err := ctx.Err(); err != nil {
		return capabilityReservation{}, err
	}
	file, err := openFileNoFollow(path, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return capabilityReservation{}, ErrNotFound
	}
	if err != nil {
		return capabilityReservation{}, fmt.Errorf("read artifact reservation record: %w", err)
	}
	defer file.Close() //nolint:errcheck
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > reservationRecordMaxBytes {
		return capabilityReservation{}, ErrCorrupt
	}
	data, err := io.ReadAll(io.LimitReader(file, reservationRecordMaxBytes+1))
	if err != nil || len(data) > reservationRecordMaxBytes {
		return capabilityReservation{}, ErrCorrupt
	}
	var record capabilityReservation
	if err := decodeStrictJSON(data, &record); err != nil || validateCapabilityReservation(record) != nil {
		return capabilityReservation{}, ErrCorrupt
	}
	return record, nil
}

func (s *reservationStore) pathForOperation(operationID string) string {
	return filepath.Join(s.dir, operationRecordName(operationID))
}

func validateCapabilityReservation(record capabilityReservation) error {
	if err := record.Request.Validate(); err != nil {
		return err
	}
	if record.ExpiresAt.IsZero() || record.ExpiresAt.Location() != time.UTC {
		return ErrCorrupt
	}
	return nil
}

func newRetirementStore(root string) (*retirementStore, error) {
	dir := filepath.Join(root, "retirements")
	if err := ensurePrivateDirectory(dir); err != nil {
		return nil, err
	}
	return &retirementStore{dir: dir}, nil
}

func (s *retirementStore) isRetired(ctx context.Context, identity Identity) (bool, error) {
	if err := identity.Validate(); err != nil {
		return false, err
	}
	_, err := s.get(ctx, identityStorageKey(identity))
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s *retirementStore) record(ctx context.Context, identity Identity, retiredAt time.Time) error {
	key := identityStorageKey(identity)
	existing, err := s.get(ctx, key)
	if err == nil {
		if existing.Identity != identity {
			return ErrCorrupt
		}
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	record := identityRetirement{Identity: identity, RetiredAt: retiredAt.UTC(), PurgeAfter: retiredAt.UTC().Add(MinimumRetirementGrace)}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(s.dir, ".retirement-*.tmp")
	if err != nil {
		return fmt.Errorf("create artifact retirement temp record: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath) //nolint:errcheck
	if err = temp.Chmod(0o600); err == nil {
		_, err = temp.Write(data)
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("persist artifact retirement record: %w", err)
	}
	if err := os.Link(tempPath, s.pathForKey(key)); err != nil {
		if errors.Is(err, os.ErrExist) {
			existing, readErr := s.get(ctx, key)
			if readErr != nil || existing.Identity != identity {
				return ErrCorrupt
			}
			return nil
		}
		return fmt.Errorf("commit artifact retirement record: %w", err)
	}
	return syncDirectory(s.dir)
}

func (s *retirementStore) get(ctx context.Context, key string) (identityRetirement, error) {
	if err := ctx.Err(); err != nil {
		return identityRetirement{}, err
	}
	file, err := openFileNoFollow(s.pathForKey(key), os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return identityRetirement{}, ErrNotFound
	}
	if err != nil {
		return identityRetirement{}, fmt.Errorf("read artifact retirement record: %w", err)
	}
	defer file.Close() //nolint:errcheck
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > retirementRecordMaxBytes {
		return identityRetirement{}, ErrCorrupt
	}
	data, err := io.ReadAll(io.LimitReader(file, retirementRecordMaxBytes+1))
	if err != nil || len(data) > retirementRecordMaxBytes {
		return identityRetirement{}, ErrCorrupt
	}
	var record identityRetirement
	if err := decodeStrictJSON(data, &record); err != nil || validateIdentityRetirement(record) != nil || identityStorageKey(record.Identity) != key {
		return identityRetirement{}, ErrCorrupt
	}
	return record, nil
}

func (s *retirementStore) list(ctx context.Context) (map[string]identityRetirement, error) {
	if err := ensurePrivateDirectory(s.dir); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("list artifact retirement records: %w", err)
	}
	result := make(map[string]identityRetirement, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".retirement-") && strings.HasSuffix(name, ".tmp") {
			if _, err := removeManagedRegularFile(filepath.Join(s.dir, name)); err != nil {
				return nil, err
			}
			continue
		}
		if entry.IsDir() || len(name) != sha256.Size*2+len(".json") || !strings.HasSuffix(name, ".json") {
			return nil, ErrCorrupt
		}
		key := strings.TrimSuffix(name, ".json")
		if _, err := hex.DecodeString(key); err != nil {
			return nil, ErrCorrupt
		}
		record, err := s.get(ctx, key)
		if err != nil {
			return nil, err
		}
		result[key] = record
	}
	return result, nil
}

func (s *retirementStore) pathForKey(key string) string {
	return filepath.Join(s.dir, key+".json")
}

func validateIdentityRetirement(record identityRetirement) error {
	if err := record.Identity.Validate(); err != nil {
		return err
	}
	if record.RetiredAt.IsZero() || record.PurgeAfter.IsZero() || record.RetiredAt.Location() != time.UTC || record.PurgeAfter.Location() != time.UTC ||
		record.PurgeAfter.Before(record.RetiredAt.Add(MinimumRetirementGrace)) {
		return ErrCorrupt
	}
	return nil
}

func identityStorageKey(identity Identity) string {
	kind := "publication"
	value := identity.PublicationID
	if identity.TaskID != "" {
		kind = "task"
		value = identity.TaskID
	}
	sum := sha256.Sum256([]byte(identity.Namespace + "\x00" + kind + "\x00" + value))
	return hex.EncodeToString(sum[:])
}

func readOperationRecordFile(path string) (OperationRecord, error) {
	file, err := openFileNoFollow(path, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return OperationRecord{}, ErrNotFound
	}
	if err != nil {
		return OperationRecord{}, fmt.Errorf("read artifact operation record: %w", err)
	}
	defer file.Close() //nolint:errcheck
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > operationRecordMaxBytes {
		return OperationRecord{}, ErrCorrupt
	}
	data, err := io.ReadAll(io.LimitReader(file, operationRecordMaxBytes+1))
	if err != nil || len(data) > operationRecordMaxBytes {
		return OperationRecord{}, ErrCorrupt
	}
	var record OperationRecord
	if err := decodeStrictJSON(data, &record); err != nil || validateOperationRecord(record, record.State) != nil {
		return OperationRecord{}, ErrCorrupt
	}
	return record, nil
}

func removeManagedRegularFile(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, ErrUnsafePath
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	return true, nil
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
