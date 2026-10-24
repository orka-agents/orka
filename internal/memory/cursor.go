package memory

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/orka-agents/orka/internal/store"
	memoryv1 "github.com/orka-agents/orka/pkg/memory/v1"
)

const (
	remoteSearchCursorTTL               = 5 * time.Minute
	maxRemoteSearchCursorBytes          = store.MaxMemorySearchCursorStateBytes
	remoteSearchSeenRecordStateVersion  = byte(1)
	remoteSearchSeenRecordDigestBytes   = sha256.Size
	remoteSearchSeenRecordDigestMaximum = memoryv1.MaxSnapshotRecords
	remoteSearchCursorPrefix            = "msc1-"
	legacyRemoteSearchCursorPrefix      = "msc-"
	legacySearchCursorPrefix            = "lsc-"
	legacySearchCursorActiveRows        = 128
	legacySearchCursorActiveBytes       = 2 << 20
	legacySearchCursorReplayRows        = 1_024
	legacySearchCursorReplayBytes       = 32 << 20
)

var (
	errLegacySearchCursorInvalid    = errors.New("invalid legacy memory search cursor")
	errLegacyRemoteSearchCursor     = errors.New("memory search cursor predates exact identity tracking")
	errRemoteSearchIdentityCapacity = errors.New("memory search identity tracking capacity reached")
)

type memorySearchCursorStore interface {
	SaveMemorySearchCursor(context.Context, store.MemorySearchCursorState) error
	GetMemorySearchCursor(context.Context, string, string, time.Time) (*store.MemorySearchCursorState, error)
	RetireMemorySearchCursor(context.Context, string, string, time.Time) error
}

type serviceLegacyCursorStore struct{ service *Service }

func (s *Service) legacySearchCursorStore() memorySearchCursorStore {
	if s != nil && s.Governed != nil {
		return s.Governed
	}
	return serviceLegacyCursorStore{service: s}
}

func (s serviceLegacyCursorStore) SaveMemorySearchCursor(_ context.Context, cursor store.MemorySearchCursorState) error {
	if s.service == nil || strings.TrimSpace(cursor.ID) == "" || strings.TrimSpace(cursor.NamespaceUID) == "" ||
		len(cursor.State) == 0 || len(cursor.State) > maxRemoteSearchCursorBytes || !cursor.ExpiresAt.After(cursor.CreatedAt) {
		return store.ErrValidation
	}
	s.service.legacyCursorMu.Lock()
	defer s.service.legacyCursorMu.Unlock()
	if s.service.legacyCursors == nil {
		s.service.legacyCursors = make(map[string]store.MemorySearchCursorState)
	}
	if s.service.legacyCursorRetired == nil {
		s.service.legacyCursorRetired = make(map[string]bool)
	}
	activeRows, activeBytes := 0, 0
	for id, existing := range s.service.legacyCursors {
		if !existing.ExpiresAt.After(cursor.CreatedAt) {
			delete(s.service.legacyCursors, id)
			delete(s.service.legacyCursorRetired, id)
			continue
		}
		if !s.service.legacyCursorRetired[id] {
			activeRows++
			activeBytes += len(existing.State)
		}
	}
	if activeRows >= legacySearchCursorActiveRows ||
		activeBytes+len(cursor.State) > legacySearchCursorActiveBytes {
		return store.ErrCapacity
	}
	copy := cursor
	copy.State = append([]byte(nil), cursor.State...)
	s.service.legacyCursors[cursor.ID] = copy
	return nil
}

func (s serviceLegacyCursorStore) RetireMemorySearchCursor(
	_ context.Context,
	namespaceUID, id string,
	now time.Time,
) error {
	if s.service == nil || strings.TrimSpace(namespaceUID) == "" || strings.TrimSpace(id) == "" {
		return store.ErrValidation
	}
	s.service.legacyCursorMu.Lock()
	defer s.service.legacyCursorMu.Unlock()
	if s.service.legacyCursorRetired == nil {
		s.service.legacyCursorRetired = make(map[string]bool)
	}
	replayRows, replayBytes := 0, 0
	for cursorID, existing := range s.service.legacyCursors {
		if !existing.ExpiresAt.After(now) {
			delete(s.service.legacyCursors, cursorID)
			delete(s.service.legacyCursorRetired, cursorID)
			continue
		}
		if s.service.legacyCursorRetired[cursorID] {
			replayRows++
			replayBytes += len(existing.State)
		}
	}
	cursor, ok := s.service.legacyCursors[id]
	if !ok || cursor.NamespaceUID != namespaceUID {
		return store.ErrNotFound
	}
	if s.service.legacyCursorRetired[id] {
		return nil
	}
	if replayRows >= legacySearchCursorReplayRows ||
		replayBytes+len(cursor.State) > legacySearchCursorReplayBytes {
		return store.ErrCapacity
	}
	s.service.legacyCursorRetired[id] = true
	return nil
}

func (s serviceLegacyCursorStore) GetMemorySearchCursor(
	_ context.Context,
	namespaceUID, id string,
	now time.Time,
) (*store.MemorySearchCursorState, error) {
	if s.service == nil {
		return nil, store.ErrNotFound
	}
	s.service.legacyCursorMu.Lock()
	defer s.service.legacyCursorMu.Unlock()
	cursor, ok := s.service.legacyCursors[strings.TrimSpace(id)]
	if !ok || cursor.NamespaceUID != strings.TrimSpace(namespaceUID) || !cursor.ExpiresAt.After(now) {
		return nil, store.ErrNotFound
	}
	copy := cursor
	copy.State = append([]byte(nil), cursor.State...)
	return &copy, nil
}

type legacySearchCursor struct {
	PageSize        int       `json:"z"`
	MACKey          []byte    `json:"k"`
	CursorID        string    `json:"-"`
	BeforeUpdatedAt time.Time `json:"-"`
	BeforeID        string    `json:"-"`
	PendingIDs      []string  `json:"-"`
}

type legacySearchCursorBoundary struct {
	BeforeUpdatedAt time.Time `json:"t"`
	BeforeID        string    `json:"i"`
	PendingIDs      []string  `json:"p,omitempty"`
}

func saveLegacySearchCursor(
	ctx context.Context,
	governed memorySearchCursorStore,
	authority *ResolvedAuthority,
	queryDigest string,
	state legacySearchCursor,
	now time.Time,
) (string, error) {
	state.BeforeID = strings.TrimSpace(state.BeforeID)
	namespaceUID := legacySearchCursorNamespace(authority)
	if governed == nil || authority == nil || namespaceUID == "" ||
		strings.TrimSpace(queryDigest) == "" || state.PageSize <= 0 || state.PageSize > maxRemoteCatalogLimit ||
		state.BeforeUpdatedAt.IsZero() || state.BeforeID == "" || len(state.PendingIDs) > state.PageSize {
		return "", errLegacySearchCursorInvalid
	}
	if err := validateLegacySearchPendingIDs(state.PendingIDs); err != nil {
		return "", err
	}
	if state.CursorID == "" {
		state.CursorID = legacySearchCursorPrefix + uuid.NewString()
		state.MACKey = make([]byte, sha256.Size)
		if _, err := rand.Read(state.MACKey); err != nil {
			return "", errLegacySearchCursorInvalid
		}
		payload, err := json.Marshal(state)
		if err != nil || len(payload) == 0 || len(payload) > maxRemoteSearchCursorBytes {
			return "", errLegacySearchCursorInvalid
		}
		err = governed.SaveMemorySearchCursor(ctx, store.MemorySearchCursorState{
			ID: state.CursorID, NamespaceUID: namespaceUID,
			BindingDigest: legacySearchBindingDigest(authority), QueryDigest: queryDigest,
			State: payload, CreatedAt: now, ExpiresAt: now.Add(remoteSearchCursorTTL),
		})
		if err != nil {
			return "", err
		}
	}
	if len(state.MACKey) != sha256.Size {
		return "", errLegacySearchCursorInvalid
	}
	return formatLegacySearchCursor(state)
}

func loadLegacySearchCursor(
	ctx context.Context,
	governed memorySearchCursorStore,
	authority *ResolvedAuthority,
	queryDigest, encoded string,
	now time.Time,
) (legacySearchCursor, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return legacySearchCursor{}, nil
	}
	if len(encoded) > 2*maxRemoteSearchCursorBytes || !strings.HasPrefix(encoded, legacySearchCursorPrefix) {
		return legacySearchCursor{}, errLegacySearchCursorInvalid
	}
	parts := strings.SplitN(encoded, ".", 4)
	namespaceUID := legacySearchCursorNamespace(authority)
	if governed == nil || authority == nil || namespaceUID == "" || len(parts) != 3 {
		return legacySearchCursor{}, errLegacySearchCursorInvalid
	}
	boundaryPayload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(boundaryPayload) == 0 || len(boundaryPayload) > maxRemoteSearchCursorBytes {
		return legacySearchCursor{}, errLegacySearchCursorInvalid
	}
	providedMAC, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(providedMAC) != sha256.Size {
		return legacySearchCursor{}, errLegacySearchCursorInvalid
	}
	stored, err := governed.GetMemorySearchCursor(ctx, namespaceUID, parts[0], now)
	if err != nil {
		return legacySearchCursor{}, err
	}
	if stored.BindingDigest != legacySearchBindingDigest(authority) || stored.QueryDigest != strings.TrimSpace(queryDigest) ||
		len(stored.State) == 0 || len(stored.State) > maxRemoteSearchCursorBytes {
		return legacySearchCursor{}, errLegacySearchCursorInvalid
	}
	var cursor legacySearchCursor
	if err := json.Unmarshal(stored.State, &cursor); err != nil || cursor.PageSize <= 0 ||
		cursor.PageSize > maxRemoteCatalogLimit || len(cursor.MACKey) != sha256.Size {
		return legacySearchCursor{}, errLegacySearchCursorInvalid
	}
	expectedMAC := legacySearchCursorMAC(parts[0], cursor.MACKey, boundaryPayload)
	if !hmac.Equal(providedMAC, expectedMAC) {
		return legacySearchCursor{}, errLegacySearchCursorInvalid
	}
	var boundary legacySearchCursorBoundary
	if err := json.Unmarshal(boundaryPayload, &boundary); err != nil || boundary.BeforeUpdatedAt.IsZero() ||
		strings.TrimSpace(boundary.BeforeID) == "" || len(boundary.PendingIDs) > cursor.PageSize {
		return legacySearchCursor{}, errLegacySearchCursorInvalid
	}
	if err := validateLegacySearchPendingIDs(boundary.PendingIDs); err != nil {
		return legacySearchCursor{}, err
	}
	cursor.CursorID = parts[0]
	cursor.BeforeUpdatedAt = boundary.BeforeUpdatedAt.UTC()
	cursor.BeforeID = strings.TrimSpace(boundary.BeforeID)
	cursor.PendingIDs = append([]string(nil), boundary.PendingIDs...)
	return cursor, nil
}

func validateLegacySearchPendingIDs(ids []string) error {
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			return errLegacySearchCursorInvalid
		}
		if _, duplicate := seen[id]; duplicate {
			return errLegacySearchCursorInvalid
		}
		seen[id] = struct{}{}
	}
	return nil
}

func formatLegacySearchCursor(state legacySearchCursor) (string, error) {
	boundary, err := json.Marshal(legacySearchCursorBoundary{
		BeforeUpdatedAt: state.BeforeUpdatedAt.UTC(), BeforeID: strings.TrimSpace(state.BeforeID),
		PendingIDs: append([]string(nil), state.PendingIDs...),
	})
	if err != nil || len(boundary) == 0 || len(boundary) > maxRemoteSearchCursorBytes {
		return "", errLegacySearchCursorInvalid
	}
	encoded := state.CursorID + "." + base64.RawURLEncoding.EncodeToString(boundary) + "." +
		base64.RawURLEncoding.EncodeToString(legacySearchCursorMAC(state.CursorID, state.MACKey, boundary))
	if len(encoded) > 2*maxRemoteSearchCursorBytes {
		return "", errLegacySearchCursorInvalid
	}
	return encoded, nil
}

func legacySearchCursorMAC(id string, key, boundary []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(id))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(boundary)
	return mac.Sum(nil)
}

func legacySearchBindingDigest(authority *ResolvedAuthority) string {
	if authority == nil {
		return ""
	}
	return digestJSON(struct {
		Mode         string `json:"mode"`
		Namespace    string `json:"namespace"`
		NamespaceUID string `json:"namespaceUid"`
	}{Mode: "legacy", Namespace: authority.Namespace, NamespaceUID: authority.NamespaceUID})
}

func legacySearchCursorNamespace(authority *ResolvedAuthority) string {
	if authority == nil {
		return ""
	}
	if namespaceUID := strings.TrimSpace(authority.NamespaceUID); namespaceUID != "" {
		return namespaceUID
	}
	if namespace := strings.TrimSpace(authority.Namespace); namespace != "" {
		return "legacy:" + namespace
	}
	return ""
}

type remoteSearchCursorRecord struct {
	MemoryID        string  `json:"m"`
	UpsertKey       string  `json:"u"`
	Generation      uint64  `json:"g"`
	BackendVersion  string  `json:"v"`
	BackendMemoryID string  `json:"i"`
	ContentDigest   string  `json:"d"`
	Score           float64 `json:"s,omitempty"`
}

// remoteSearchCursorMatch preserves verified results withheld by strict mode.
// Live records retain only a bounded ID, score, and complete canonical record
// digest before exact re-fetch; tombstones are revalidated from the local catalog.
type remoteSearchCursorMatch struct {
	MemoryID     string  `json:"m"`
	RecordDigest string  `json:"d,omitempty"`
	Score        float64 `json:"s,omitempty"`
	Tombstone    bool    `json:"t,omitempty"`
}

type remoteSearchCursor struct {
	ProviderToken     string                     `json:"p"`
	ProviderExhausted bool                       `json:"x,omitempty"`
	PageSize          int                        `json:"z"`
	ActualMode        string                     `json:"m,omitempty"`
	Pending           []remoteSearchCursorRecord `json:"n,omitempty"`
	ReplayPrefix      []remoteSearchCursorRecord `json:"f,omitempty"`
	PendingMatches    []remoteSearchCursorMatch  `json:"h,omitempty"`
	SeenRecordState   []byte                     `json:"s,omitempty"`
}

func remoteSearchCursorRecordForRecord(record *memoryv1.MemoryRecord) remoteSearchCursorRecord {
	if record == nil {
		return remoteSearchCursorRecord{}
	}
	return remoteSearchCursorRecord{
		MemoryID: record.MemoryID, UpsertKey: record.UpsertKey, Generation: record.Generation,
		BackendVersion: record.BackendVersion, BackendMemoryID: record.BackendMemoryID,
		ContentDigest: record.ContentDigest, Score: record.Score,
	}
}

func remoteSearchRecordMaterializationDigest(record *memoryv1.MemoryRecord, binding memoryv1.Binding) (string, error) {
	if err := memoryv1.ValidateMemoryRecord(record, binding); err != nil {
		return "", err
	}
	copy := *record
	copy.Score = 0
	return memoryv1.MemoryRecordDigest(&copy, binding)
}

func remoteSearchCursorMatchForRecord(
	record *memoryv1.MemoryRecord,
	binding memoryv1.Binding,
) (remoteSearchCursorMatch, error) {
	digest, err := remoteSearchRecordMaterializationDigest(record, binding)
	if err != nil {
		return remoteSearchCursorMatch{}, err
	}
	return remoteSearchCursorMatch{MemoryID: record.MemoryID, RecordDigest: digest, Score: record.Score}, nil
}

func cloneRemoteSearchCursorMatches(matches []remoteSearchCursorMatch) []remoteSearchCursorMatch {
	return append([]remoteSearchCursorMatch(nil), matches...)
}

func remoteSearchCursorMatchesRequireSnapshot(matches []remoteSearchCursorMatch) bool {
	for i := range matches {
		if !matches[i].Tombstone {
			return true
		}
	}
	return false
}

func validRemoteSearchDigest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, value := range value[len("sha256:"):] {
		if (value < '0' || value > '9') && (value < 'a' || value > 'f') {
			return false
		}
	}
	return true
}

func validRemoteSearchCursorMatches(matches []remoteSearchCursorMatch, _ memoryv1.Binding) bool {
	if len(matches) > maxRemoteCatalogLimit {
		return false
	}
	seen := make(map[string]struct{}, len(matches))
	for i := range matches {
		match := &matches[i]
		if strings.TrimSpace(match.MemoryID) != match.MemoryID || match.MemoryID == "" ||
			len(match.MemoryID) > memoryv1.MaxIdentityBytes || math.IsNaN(match.Score) || math.IsInf(match.Score, 0) || match.Score < 0 ||
			match.Tombstone && (match.RecordDigest != "" || match.Score != 0) ||
			!match.Tombstone && !validRemoteSearchDigest(match.RecordDigest) {
			return false
		}
		if _, duplicate := seen[match.MemoryID]; duplicate {
			return false
		}
		seen[match.MemoryID] = struct{}{}
	}
	return true
}

func newRemoteSearchSeenRecordState() []byte {
	return []byte{remoteSearchSeenRecordStateVersion}
}

func validRemoteSearchSeenRecordState(state []byte) bool {
	if len(state) == 0 {
		return true
	}
	payloadBytes := len(state) - 1
	return state[0] == remoteSearchSeenRecordStateVersion &&
		payloadBytes%remoteSearchSeenRecordDigestBytes == 0 &&
		payloadBytes/remoteSearchSeenRecordDigestBytes <= remoteSearchSeenRecordDigestMaximum
}

func remoteSearchSeenRecordStatePresent(state []byte) bool {
	return len(state) > 0
}

func remoteSearchSeenRecordCount(state []byte) (int, bool) {
	if len(state) == 0 {
		return 0, true
	}
	if !validRemoteSearchSeenRecordState(state) {
		return 0, false
	}
	return (len(state) - 1) / remoteSearchSeenRecordDigestBytes, true
}

func cloneRemoteSearchSeenRecordState(state []byte) []byte {
	return append([]byte(nil), state...)
}

// rememberRemoteSearchRecordIdentity records one canonical upsert key as a
// complete SHA-256 digest. The state is exact within the protocol's bounded
// snapshot cardinality; reaching that hard bound returns capacity rather than
// probabilistically classifying a fresh provider record as a duplicate.
func rememberRemoteSearchRecordIdentity(state *[]byte, upsertKey string) (bool, error) {
	if state == nil || strings.TrimSpace(upsertKey) == "" {
		return false, errors.New("memory search record identity is invalid")
	}
	if len(*state) == 0 {
		*state = newRemoteSearchSeenRecordState()
	}
	if !validRemoteSearchSeenRecordState(*state) || !remoteSearchSeenRecordStatePresent(*state) {
		return false, errors.New("memory search identity state is invalid")
	}
	digest := sha256.Sum256([]byte(upsertKey))
	for offset := 1; offset < len(*state); offset += remoteSearchSeenRecordDigestBytes {
		if bytes.Equal((*state)[offset:offset+remoteSearchSeenRecordDigestBytes], digest[:]) {
			return true, nil
		}
	}
	count := (len(*state) - 1) / remoteSearchSeenRecordDigestBytes
	if count >= remoteSearchSeenRecordDigestMaximum {
		return false, errRemoteSearchIdentityCapacity
	}
	*state = append(*state, digest[:]...)
	return false, nil
}

func saveRemoteSearchCursor(
	ctx context.Context,
	governed store.GovernedMemoryStore,
	binding *store.MemoryBackendBinding,
	queryDigest string,
	state remoteSearchCursor,
	now time.Time,
) (string, error) {
	if governed == nil || binding == nil || (state.ProviderToken == "" && len(state.Pending) == 0) ||
		!remoteSearchSeenRecordStatePresent(state.SeenRecordState) {
		return "", errors.New("memory search cursor state is unavailable")
	}
	identity, err := protocolBinding(binding)
	if err != nil {
		return "", err
	}
	if !validRemoteSearchCursorMatches(state.PendingMatches, identity) {
		return "", errors.New("memory search cursor matches are invalid")
	}
	payload, err := json.Marshal(state)
	if err != nil || len(payload) == 0 || len(payload) > maxRemoteSearchCursorBytes {
		return "", errors.New("memory search cursor state is invalid")
	}
	id := remoteSearchCursorPrefix + uuid.NewString()
	err = governed.SaveMemorySearchCursor(ctx, store.MemorySearchCursorState{
		ID: id, NamespaceUID: binding.NamespaceUID,
		BindingDigest: memoryv1.BindingDigest(identity), QueryDigest: queryDigest,
		State: payload, CreatedAt: now, ExpiresAt: now.Add(remoteSearchCursorTTL),
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

func loadRemoteSearchCursor(
	ctx context.Context,
	governed store.GovernedMemoryStore,
	binding *store.MemoryBackendBinding,
	queryDigest, encoded string,
	now time.Time,
) (remoteSearchCursor, error) {
	if strings.TrimSpace(encoded) == "" {
		return remoteSearchCursor{}, nil
	}
	encoded = strings.TrimSpace(encoded)
	if strings.HasPrefix(encoded, legacyRemoteSearchCursorPrefix) && !strings.HasPrefix(encoded, remoteSearchCursorPrefix) {
		return remoteSearchCursor{}, errLegacyRemoteSearchCursor
	}
	if governed == nil || binding == nil || !strings.HasPrefix(encoded, remoteSearchCursorPrefix) || len(encoded) > 128 {
		return remoteSearchCursor{}, errors.New("invalid memory search cursor")
	}
	identity, err := protocolBinding(binding)
	if err != nil {
		return remoteSearchCursor{}, err
	}
	stored, err := governed.GetMemorySearchCursor(ctx, binding.NamespaceUID, encoded, now)
	if err != nil {
		return remoteSearchCursor{}, err
	}
	if stored.BindingDigest != memoryv1.BindingDigest(identity) || stored.QueryDigest != queryDigest ||
		len(stored.State) == 0 || len(stored.State) > maxRemoteSearchCursorBytes {
		return remoteSearchCursor{}, errors.New("mismatched memory search cursor")
	}
	var cursor remoteSearchCursor
	if err := json.Unmarshal(stored.State, &cursor); err != nil || cursor.PageSize <= 0 ||
		cursor.PageSize > memoryv1.MaxPageSize ||
		(cursor.ActualMode != memoryv1.SearchModeKeyword && cursor.ActualMode != memoryv1.SearchModeSemantic &&
			cursor.ActualMode != memoryv1.SearchModeHybrid) ||
		(cursor.ProviderToken == "" && len(cursor.Pending) == 0) || len(cursor.Pending) > memoryv1.MaxPageSize ||
		!validRemoteSearchCursorMatches(cursor.PendingMatches, identity) ||
		!validRemoteSearchSeenRecordState(cursor.SeenRecordState) ||
		!remoteSearchSeenRecordStatePresent(cursor.SeenRecordState) {
		return remoteSearchCursor{}, errors.New("invalid memory search cursor state")
	}
	return cursor, nil
}
