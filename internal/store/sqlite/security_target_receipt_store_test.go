package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/store"
)

const (
	testTargetHeadOID   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testSnapshotDigest  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testTargetObjectFmt = "sha1"
)

func testSecurityTargetReceiptJSON(
	t *testing.T,
	headOID, snapshotDigest, treeDigest string,
	overrides map[string]any,
) json.RawMessage {
	t.Helper()
	payload := map[string]any{
		"headOID":        headOID,
		"objectFormat":   testTargetObjectFmt,
		"snapshotDigest": snapshotDigest,
		"treeDigest":     treeDigest,
	}
	maps.Copy(payload, overrides)
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSecurityTargetReceiptImmutableReplay(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	headOID := testTargetHeadOID
	snapshotDigest := testSnapshotDigest
	treeDigest := testSHA256DigestB
	receipt := &store.SecurityTargetReceipt{
		ID:        "target_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Namespace: "ns", RepositoryScan: "repo", ScanRunID: "scan", RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TargetID: "repo_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		HeadSHA:  headOID, ObjectFormat: testTargetObjectFmt,
		SnapshotDigest: snapshotDigest,
		TreeDigest:     treeDigest,
		ReceiptJSON:    testSecurityTargetReceiptJSON(t, headOID, snapshotDigest, treeDigest, nil),
	}
	created, err := s.SaveSecurityTargetReceipt(ctx, receipt)
	if err != nil || !created {
		t.Fatalf("SaveSecurityTargetReceipt(first) = %v, %v", created, err)
	}
	created, err = s.SaveSecurityTargetReceipt(ctx, receipt)
	if err != nil || created {
		t.Fatalf("SaveSecurityTargetReceipt(replay) = %v, %v", created, err)
	}
	conflict := *receipt
	conflict.ReceiptJSON = testSecurityTargetReceiptJSON(t, headOID, snapshotDigest, treeDigest, map[string]any{"note": "different immutable payload"})
	conflict.PayloadDigest = ""
	if _, err := s.SaveSecurityTargetReceipt(ctx, &conflict); !errors.Is(err, store.ErrDuplicateMismatch) {
		t.Fatalf("conflicting replay error = %v", err)
	}
	got, err := s.GetSecurityTargetReceipt(ctx, "ns", receipt.ID)
	if err != nil || got.HeadSHA != receipt.HeadSHA {
		t.Fatalf("GetSecurityTargetReceipt() = %#v, %v", got, err)
	}
	if _, err := s.db.Exec(`UPDATE security_target_receipts SET head_sha = 'x' WHERE id = ?`, receipt.ID); err == nil {
		t.Fatal("immutable target receipt update error = nil")
	}
}

func TestSecurityTargetReceiptInventoryReadCopiesBlob(t *testing.T) {
	s := setupTestStore(t)
	inventory := json.RawMessage(`{"schemaVersion":2,"files":["main.go"]}`)
	receipt := &store.SecurityTargetReceipt{
		ID:             "target_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Namespace:      "ns",
		RepositoryScan: "repo",
		ScanRunID:      "scan",
		RunUID:         "run_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		TargetID:       "repo_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		HeadSHA:        testTargetHeadOID,
		ObjectFormat:   testTargetObjectFmt,
		SnapshotDigest: testSnapshotDigest,
		TreeDigest:     testSHA256DigestB,
		ReceiptJSON:    testSecurityTargetReceiptJSON(t, testTargetHeadOID, testSnapshotDigest, testSHA256DigestB, nil),
		InventoryJSON:  inventory,
	}
	if created, err := s.SaveSecurityTargetReceipt(context.Background(), receipt); err != nil || !created {
		t.Fatalf("SaveSecurityTargetReceipt() = (%v, %v)", created, err)
	}

	first, err := s.GetSecurityTargetReceipt(context.Background(), receipt.Namespace, receipt.ID)
	if err != nil {
		t.Fatalf("GetSecurityTargetReceipt(first) error = %v", err)
	}
	wantInventory := string(receipt.InventoryJSON)
	if string(first.InventoryJSON) != wantInventory {
		t.Fatalf("InventoryJSON = %q, want %q", first.InventoryJSON, wantInventory)
	}
	first.InventoryJSON[0] = 'x'
	first.ReceiptJSON[0] = 'x'

	second, err := s.GetSecurityTargetReceipt(context.Background(), receipt.Namespace, receipt.ID)
	if err != nil {
		t.Fatalf("GetSecurityTargetReceipt(second) error = %v", err)
	}
	if string(second.InventoryJSON) != wantInventory || string(second.ReceiptJSON) != string(receipt.ReceiptJSON) {
		t.Fatalf("second read reused mutable scan buffers: receipt=%q inventory=%q", second.ReceiptJSON, second.InventoryJSON)
	}
}

func TestSecurityTargetReceiptRejectsReceiptJSONBindingContradictions(t *testing.T) {
	headOID := testTargetHeadOID
	snapshotDigest := testSnapshotDigest
	treeDigest := testSHA256DigestB
	tests := map[string]map[string]any{
		"head OID":        {"headOID": "cccccccccccccccccccccccccccccccccccccccc"},
		"object format":   {"objectFormat": "sha256"},
		"snapshot digest": {"snapshotDigest": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},
		"tree digest":     {"treeDigest": "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"},
	}
	for name, overrides := range tests {
		t.Run(name, func(t *testing.T) {
			s := setupTestStore(t)
			receipt := &store.SecurityTargetReceipt{
				ID:        "target_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Namespace: "ns", RepositoryScan: "repo", ScanRunID: "scan", RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				TargetID: "repo_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				HeadSHA:  headOID, ObjectFormat: testTargetObjectFmt,
				SnapshotDigest: snapshotDigest,
				TreeDigest:     treeDigest,
				ReceiptJSON:    testSecurityTargetReceiptJSON(t, headOID, snapshotDigest, treeDigest, overrides),
			}
			if _, err := s.SaveSecurityTargetReceipt(context.Background(), receipt); !errors.Is(err, store.ErrValidation) {
				t.Fatalf("SaveSecurityTargetReceipt() error = %v, want ErrValidation", err)
			}
			if _, err := s.GetSecurityTargetReceipt(context.Background(), "ns", receipt.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetSecurityTargetReceipt() error = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestSecurityTargetReceiptRejectsInvalidObjectID(t *testing.T) {
	s := setupTestStore(t)
	receipt := &store.SecurityTargetReceipt{
		ID: "target_invalid", Namespace: "ns", RepositoryScan: "repo", ScanRunID: "scan",
		RunUID:   "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TargetID: "repo_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		HeadSHA:  "x", ObjectFormat: testTargetObjectFmt,
		SnapshotDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TreeDigest:     "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ReceiptJSON:    []byte(`{"headOID":"x"}`),
	}
	if _, err := s.SaveSecurityTargetReceipt(context.Background(), receipt); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("invalid object ID error = %v", err)
	}
}

func TestSecurityTargetReceiptRejectsCredentialContentBeforePersistence(t *testing.T) {
	tests := map[string]string{
		"authorization header": strings.Join([]string{"Author", "ization: Bearer ", "value-for-redaction"}, ""),
		"transaction header":   strings.Join([]string{"Txn", "-To", "ken: ", "value-for-redaction"}, ""),
		"sensitive assignment": strings.Join([]string{"api", "_key=", "sk", "-abcdefghijklmnopqrstuvwxyz123456"}, ""),
		"github token":         strings.Join([]string{"github", "_pat_", "abcdefghijklmnopqrstuvwxyz1234567890"}, ""),
		"jwt":                  strings.Join([]string{"eyJhbGciOiJSUzI1NiJ9", "eyJzdWIiOiJ3b3JrbG9hZCJ9", "signaturevalue1234567890"}, "."),
		"credentialed url":     strings.Join([]string{"https://username:", "pass", "word@example.com/org/repo.git"}, ""),
	}
	for name, credentialLike := range tests {
		for _, field := range []string{"receiptJSON", "inventoryJSON"} {
			t.Run(name+"_"+field, func(t *testing.T) {
				s := setupTestStore(t)
				receiptPayload := json.RawMessage(`{"headOID":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
				inventoryPayload := json.RawMessage(`{"schemaVersion":2,"summary":"safe"}`)
				credentialPayload, err := json.Marshal(map[string]string{"note": credentialLike})
				if err != nil {
					t.Fatal(err)
				}
				if field == "receiptJSON" {
					receiptPayload = credentialPayload
				} else {
					inventoryPayload = credentialPayload
				}
				receipt := &store.SecurityTargetReceipt{
					ID:        "target_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					Namespace: "ns", RepositoryScan: "repo", ScanRunID: "scan", RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					TargetID: "repo_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					HeadSHA:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ObjectFormat: testTargetObjectFmt,
					SnapshotDigest:  "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					TreeDigest:      "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
					ReceiptJSON:     receiptPayload,
					InventoryJSON:   inventoryPayload,
					PayloadDigest:   securityDigestBytes(receiptPayload),
					InventoryDigest: securityDigestBytes(inventoryPayload),
				}
				if _, err := s.SaveSecurityTargetReceipt(context.Background(), receipt); !errors.Is(err, store.ErrValidation) {
					t.Fatalf("SaveSecurityTargetReceipt() error = %v, want ErrValidation", err)
				}
				if _, err := s.GetSecurityTargetReceipt(context.Background(), "ns", receipt.ID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("GetSecurityTargetReceipt() error = %v, want ErrNotFound", err)
				}
			})
		}
	}
}

func TestSecurityTargetReceiptRejectsInvalidUTF8JSONBeforePersistence(t *testing.T) {
	headOID := testTargetHeadOID
	snapshotDigest := testSnapshotDigest
	treeDigest := testSHA256DigestB
	invalidUTF8 := json.RawMessage([]byte{'{', '"', 'n', 'o', 't', 'e', '"', ':', '"', 0xff, '"', '}'})
	if !json.Valid(invalidUTF8) {
		t.Fatal("test payload must remain JSON-valid under encoding/json")
	}
	for _, field := range []string{"receiptJSON", "inventoryJSON"} {
		t.Run(field, func(t *testing.T) {
			s := setupTestStore(t)
			receiptJSON := testSecurityTargetReceiptJSON(t, headOID, snapshotDigest, treeDigest, nil)
			inventoryJSON := json.RawMessage(`{"schemaVersion":2}`)
			if field == "receiptJSON" {
				receiptJSON = invalidUTF8
			} else {
				inventoryJSON = invalidUTF8
			}
			receipt := &store.SecurityTargetReceipt{
				ID:        "target_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Namespace: "ns", RepositoryScan: "repo", ScanRunID: "scan", RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				TargetID: "repo_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				HeadSHA:  headOID, ObjectFormat: testTargetObjectFmt, SnapshotDigest: snapshotDigest, TreeDigest: treeDigest,
				ReceiptJSON: receiptJSON, InventoryJSON: inventoryJSON,
				PayloadDigest: securityDigestBytes(receiptJSON), InventoryDigest: securityDigestBytes(inventoryJSON),
			}
			if _, err := s.SaveSecurityTargetReceipt(context.Background(), receipt); !errors.Is(err, store.ErrValidation) {
				t.Fatalf("SaveSecurityTargetReceipt() error = %v, want ErrValidation", err)
			}
			if _, err := s.GetSecurityTargetReceipt(context.Background(), "ns", receipt.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetSecurityTargetReceipt() error = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestNormalizeTargetPayloadRejectsEscapedAndKeyedCredentialContent(t *testing.T) {
	tests := map[string]json.RawMessage{
		"escaped header": json.RawMessage(`{"note":"Authorizatio\u006e: Bearer value-for-redaction"}`),
		"header field":   json.RawMessage(`{"Authorization":"Bearer value-for-redaction"}`),
		"escaped url":    json.RawMessage(`{"note":"https://username:password\u0040example.com/org/repo.git"}`),
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := normalizeTargetPayload(payload, securityDigestBytes(payload), true, "receiptJSON"); !errors.Is(err, store.ErrValidation) {
				t.Fatalf("normalizeTargetPayload() error = %v, want ErrValidation", err)
			}
		})
	}
}

func TestNormalizeTargetPayloadRejectsSensitiveNonStringFields(t *testing.T) {
	tests := map[string]json.RawMessage{
		"numeric token":           json.RawMessage(`{"token":12345}`),
		"boolean API key":         json.RawMessage(`{"apiKey":false}`),
		"null password":           json.RawMessage(`{"password":null}`),
		"object client secret":    json.RawMessage(`{"clientSecret":{"present":false}}`),
		"array transaction token": json.RawMessage(`{"Txn-Token":[]}`),
		"numeric authorization":   json.RawMessage(`{"Authorization":401}`),
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := normalizeTargetPayload(payload, securityDigestBytes(payload), true, "receiptJSON"); !errors.Is(err, store.ErrValidation) {
				t.Fatalf("normalizeTargetPayload() error = %v, want ErrValidation", err)
			}
		})
	}
}

func TestNormalizeTargetPayloadAcceptsSafeNonStringFields(t *testing.T) {
	payload := json.RawMessage(`{"cleanTrackedWorktree":true,"treeEntryCount":123}`)
	if _, _, err := normalizeTargetPayload(payload, securityDigestBytes(payload), true, "receiptJSON"); err != nil {
		t.Fatalf("normalizeTargetPayload() error = %v", err)
	}
}

func TestNormalizeTargetPayloadPreservesSafeSecurityTerminology(t *testing.T) {
	payload, err := json.Marshal(map[string]string{
		"summary": "Review token-based authorization boundaries without embedding credentials.",
	})
	if err != nil {
		t.Fatal(err)
	}
	originalDigest := securityDigestBytes(payload)
	normalized, digest, err := normalizeTargetPayload(payload, originalDigest, true, "receiptJSON")
	if err != nil {
		t.Fatalf("normalizeTargetPayload() error = %v", err)
	}
	if string(normalized) != string(payload) {
		t.Fatalf("normalizeTargetPayload() changed safe terminology: %q", normalized)
	}
	if digest != originalDigest {
		t.Fatalf("digest = %q, want %q", digest, originalDigest)
	}
}
