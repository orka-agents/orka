package acp

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPrepareSessionPathsPrivateAndUnique(t *testing.T) {
	base := filepath.Join(t.TempDir(), "sessions")
	first, err := PrepareSessionPaths(base, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := PrepareSessionPaths(base, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Root == second.Root {
		t.Fatal("session roots were reused")
	}
	for _, path := range []string{first.Root, first.Home, first.Temp, first.Workspace, first.Config, first.Cache, first.Data, first.State} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("%s mode = %o, want 700", path, got)
		}
		if !strings.HasPrefix(path, first.Root) {
			t.Fatalf("path %s escaped root %s", path, first.Root)
		}
	}
}

func TestPrepareSessionPathsRemainSupervisorWritableUntilFinalization(t *testing.T) {
	paths, err := PrepareSessionPaths(filepath.Join(t.TempDir(), "sessions"), "session-writable")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.Root, paths.Home, paths.Temp, paths.Workspace, paths.Config, paths.Cache, paths.Data, paths.State} {
		probe := filepath.Join(path, "supervisor-write-probe")
		if err := os.WriteFile(probe, []byte("prepared"), 0o600); err != nil {
			t.Fatalf("session path %s was not writable before ownership finalization: %v", path, err)
		}
	}
}

func TestPrepareSessionPathsRejectsUnsafeInputs(t *testing.T) {
	for _, sessionID := range []string{"", "../escape", "nested/path", ".hidden", strings.Repeat("x", 129)} {
		if _, err := PrepareSessionPaths(t.TempDir(), sessionID); err == nil {
			t.Fatalf("session ID %q unexpectedly accepted", sessionID)
		}
	}
	symlink := filepath.Join(t.TempDir(), "link")
	target := t.TempDir()
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareSessionPaths(symlink, "session"); err == nil {
		t.Fatal("symlink base unexpectedly accepted")
	}
	after, err := os.Stat(target)
	if err != nil || after.Mode() != before.Mode() {
		t.Fatalf("rejected symlink changed target permissions: %v", err)
	}
}

func TestBuildChildEnvironmentStartsFromEmptyAllowlist(t *testing.T) {
	paths := SessionPaths{
		Home: "/sessions/a/home", Temp: "/sessions/a/tmp", Workspace: "/sessions/a/workspace",
		Config: "/sessions/a/config", Cache: "/sessions/a/cache", Data: "/sessions/a/data", State: "/sessions/a/state",
	}
	t.Setenv("SUPERVISOR_SECRET", "do-not-copy")
	env, err := BuildChildEnvironment(paths, EnvironmentConfig{Values: map[string]string{"CODEX_API_KEY": "test"}})
	if err != nil {
		t.Fatal(err)
	}
	values := environmentMap(env)
	if _, ok := values["SUPERVISOR_SECRET"]; ok {
		t.Fatal("ambient supervisor environment leaked")
	}
	if values["CODEX_API_KEY"] != "test" || values["HOME"] != paths.Home || values["PWD"] != paths.Workspace {
		t.Fatalf("unexpected environment: %#v", values)
	}
	if len(values) != len(env) {
		t.Fatalf("duplicate environment names: %v", env)
	}
}

func TestBuildChildEnvironmentDeterministicAndValidated(t *testing.T) {
	paths := SessionPaths{
		Home: "/h", Temp: "/t", Workspace: "/w", Config: "/c", Cache: "/cache", Data: "/d", State: "/s",
	}
	cfg := EnvironmentConfig{Values: map[string]string{"B": "2", "A": "1"}, UnsetNames: []string{"B"}}
	first, err := BuildChildEnvironment(paths, cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildChildEnvironment(paths, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("environment is not deterministic:\n%v\n%v", first, second)
	}
	if _, err := BuildChildEnvironment(paths, EnvironmentConfig{Values: map[string]string{"BAD-NAME": "x"}}); err == nil {
		t.Fatal("invalid environment name unexpectedly accepted")
	}
}

func TestUIDAllocatorAllocatesDistinctPairsAndExhausts(t *testing.T) {
	allocator, err := NewUIDAllocator(20000, 20002, 30000)
	if err != nil {
		t.Fatal(err)
	}
	seenUIDs := make(map[int]struct{}, allocator.Capacity())
	seenGIDs := make(map[int]struct{}, allocator.Capacity())
	for offset := range allocator.Capacity() {
		uid, gid, err := allocator.AllocateAboveReserve(0)
		if err != nil {
			t.Fatal(err)
		}
		wantUID, wantGID := 20000+offset, 30000+offset
		if uid != wantUID || gid != wantGID {
			t.Fatalf("allocation = %d:%d, want %d:%d", uid, gid, wantUID, wantGID)
		}
		if _, exists := seenUIDs[uid]; exists {
			t.Fatalf("UID %d was reused", uid)
		}
		if _, exists := seenGIDs[gid]; exists {
			t.Fatalf("GID %d was reused", gid)
		}
		seenUIDs[uid] = struct{}{}
		seenGIDs[gid] = struct{}{}
	}
	if allocator.Remaining() != 0 {
		t.Fatalf("remaining = %d, want 0", allocator.Remaining())
	}
	for range 2 {
		uid, gid, err := allocator.AllocateAboveReserve(0)
		if !errors.Is(err, ErrUIDRangeExhausted) {
			t.Fatalf("exhausted allocation error = %v, want ErrUIDRangeExhausted", err)
		}
		if uid != 0 || gid != 0 {
			t.Fatalf("exhausted allocation = %d:%d, want 0:0", uid, gid)
		}
	}
}

func TestUIDAllocatorPreservesConfiguredReserve(t *testing.T) {
	allocator, err := NewUIDAllocator(20000, 20002, 30000)
	if err != nil {
		t.Fatal(err)
	}
	if allocator.Capacity() != 3 {
		t.Fatalf("capacity = %d, want 3", allocator.Capacity())
	}
	for offset := range 2 {
		uid, gid, err := allocator.AllocateAboveReserve(1)
		if err != nil {
			t.Fatal(err)
		}
		wantUID, wantGID := 20000+offset, 30000+offset
		if uid != wantUID || gid != wantGID {
			t.Fatalf("allocation = %d:%d, want %d:%d", uid, gid, wantUID, wantGID)
		}
	}
	if _, _, err := allocator.AllocateAboveReserve(1); !errors.Is(err, ErrUIDReserveReached) {
		t.Fatalf("reserved allocation error = %v, want ErrUIDReserveReached", err)
	}
	if allocator.Remaining() != 1 {
		t.Fatalf("remaining = %d, want protected reserve of 1", allocator.Remaining())
	}
}

func TestUIDAllocatorRejectsGIDRangeThatCannotRemainDistinct(t *testing.T) {
	if _, err := NewUIDAllocator(20000, 20001, int(^uint(0)>>1)); err == nil {
		t.Fatal("allocator unexpectedly accepted a GID range that cannot represent every pair")
	}
}

func TestUIDAllocatorPersistsHighWaterBeforeReturningIdentity(t *testing.T) {
	allocator, err := NewUIDAllocator(20000, 20002, 30000)
	if err != nil {
		t.Fatal(err)
	}
	persisted := 0
	if err := allocator.ConfigurePersistence(1, func(next int) error {
		persisted = next
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	uid, gid, err := allocator.AllocateAboveReserve(0)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 20001 || gid != 30001 || persisted != 2 {
		t.Fatalf("allocation/persistence = %d:%d/%d, want 20001:30001/2", uid, gid, persisted)
	}

	failing, err := NewUIDAllocator(40000, 40001, 50000)
	if err != nil {
		t.Fatal(err)
	}
	persistErr := errors.New("persist failed")
	if err := failing.ConfigurePersistence(0, func(int) error { return persistErr }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := failing.AllocateAboveReserve(0); !errors.Is(err, persistErr) {
		t.Fatalf("allocation error = %v, want persistence failure", err)
	}
	if failing.Remaining() != failing.Capacity() {
		t.Fatalf("failed persistence consumed capacity: remaining=%d capacity=%d", failing.Remaining(), failing.Capacity())
	}
}

func TestFinalizeSessionOwnershipNonRootOwnIdentity(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		t.Skip("non-root behavior")
	}
	if err := FinalizeSessionOwnership(root, uid, gid); err != nil {
		t.Fatal(err)
	}
}

func TestSessionOwnershipFinalizationOrdersChildrenAndChmodBeforeChown(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	grandchild := filepath.Join(child, "grandchild")
	if err := os.MkdirAll(grandchild, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(grandchild, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := sessionOwnershipEntries(root)
	if err != nil {
		t.Fatal(err)
	}
	indexes := make(map[string]int, len(entries))
	for index, entry := range entries {
		indexes[entry.path] = index
	}
	for _, path := range []string{child, grandchild, file} {
		parent := filepath.Dir(path)
		if indexes[path] >= indexes[parent] {
			t.Fatalf("ownership order processes %s at %d after parent %s at %d", path, indexes[path], parent, indexes[parent])
		}
	}

	events := make([]string, 0, len(entries)*2)
	err = assignSessionOwnership(entries, 20000, 20000,
		func(path string, mode os.FileMode) error {
			if mode != 0o700 {
				t.Fatalf("chmod mode for %s = %o, want 700", path, mode)
			}
			events = append(events, "chmod:"+path)
			return nil
		},
		func(path string, uid, gid int) error {
			if uid != 20000 || gid != 20000 {
				t.Fatalf("ownership for %s = %d:%d, want 20000:20000", path, uid, gid)
			}
			events = append(events, "chown:"+path)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	eventIndex := make(map[string]int, len(events))
	for index, event := range events {
		eventIndex[event] = index
	}
	for _, entry := range entries {
		if !entry.directory {
			continue
		}
		chmodIndex, chmodOK := eventIndex["chmod:"+entry.path]
		chownIndex, chownOK := eventIndex["chown:"+entry.path]
		if !chmodOK || !chownOK || chmodIndex >= chownIndex {
			t.Fatalf("directory finalization order for %s = %v", entry.path, events)
		}
	}
}

func TestSessionOwnershipReclaimOrdersParentsAndChownBeforeChmod(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	grandchild := filepath.Join(child, "grandchild")
	if err := os.MkdirAll(grandchild, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(grandchild, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	events := make([]string, 0, 8)
	err := reclaimSessionOwnership(root, 0, 0,
		func(path string, uid, gid int) error {
			if uid != 0 || gid != 0 {
				t.Fatalf("reclaim identity for %s = %d:%d, want 0:0", path, uid, gid)
			}
			events = append(events, "chown:"+path)
			return nil
		},
		func(path string, mode os.FileMode) error {
			if mode != 0o700 {
				t.Fatalf("reclaim chmod mode for %s = %o, want 700", path, mode)
			}
			events = append(events, "chmod:"+path)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	eventIndex := make(map[string]int, len(events))
	for index, event := range events {
		eventIndex[event] = index
	}
	for _, path := range []string{child, grandchild, file} {
		parent := filepath.Dir(path)
		if eventIndex["chown:"+parent] >= eventIndex["chown:"+path] {
			t.Fatalf("reclaim order processes child %s before parent %s: %v", path, parent, events)
		}
	}
	for _, path := range []string{root, child, grandchild} {
		if eventIndex["chown:"+path] >= eventIndex["chmod:"+path] {
			t.Fatalf("reclaim order chmods %s before chown: %v", path, events)
		}
	}
}

// GitHub repository identities are case-insensitive: a continuation that
// changes only capitalization must be judged the same repository so the
// preserved durable tree resumes instead of being wiped.
func TestSameDurableWorkspaceIdentity(t *testing.T) {
	const canonicalRepo = "github.com/orka-agents/orka"
	cases := []struct {
		first, second string
		want          bool
	}{
		{"github.com/Orka-Agents/Orka", canonicalRepo, true},
		{canonicalRepo, canonicalRepo, true},
		{canonicalRepo, "github.com/orka-agents/other", false},
		{"gitlab.com/Orka/Repo", "gitlab.com/orka/repo", false},
		{"__no_workspace__", "__no_workspace__", true},
		{"", "", true},
	}
	for _, tc := range cases {
		if got := SameDurableWorkspaceIdentity(tc.first, tc.second); got != tc.want {
			t.Fatalf("SameDurableWorkspaceIdentity(%q, %q) = %v, want %v", tc.first, tc.second, got, tc.want)
		}
	}
}

func environmentMap(env []string) map[string]string {
	result := make(map[string]string, len(env))
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			result[name] = value
		}
	}
	return result
}

func TestReadOnlyWorkspaceProtectionModes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		durable      bool
		protectsRoot bool
	}{
		{name: "workspace inside session root", protectsRoot: true},
		{name: "durable workspace outside session root", durable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "session")
			workspace := filepath.Join(root, "workspace")
			if tc.durable {
				workspace = filepath.Join(t.TempDir(), "ws-session")
			}
			nested := filepath.Join(workspace, "dir")
			if err := os.MkdirAll(nested, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			files := map[string]os.FileMode{
				filepath.Join(workspace, "readme"):          0o644,
				filepath.Join(workspace, "tool"):            0o755,
				filepath.Join(nested, "group-writable"):     0o664,
				filepath.Join(nested, "private"):            0o600,
				filepath.Join(nested, "private-executable"): 0o700,
			}
			for path, mode := range files {
				if err := os.WriteFile(path, []byte("x"), mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
			}
			link := filepath.Join(workspace, "link")
			if err := os.Symlink("readme", link); err != nil {
				t.Fatal(err)
			}

			var events []string
			chowned := map[string]bool{}
			chmods := map[string]os.FileMode{}
			err := protectReadOnlySessionWorkspace(root, workspace, 0, 20000,
				func(path string, uid, gid int) error {
					if uid != 0 || gid != 20000 {
						t.Fatalf("ownership for %s = %d:%d, want 0:20000", path, uid, gid)
					}
					chowned[path] = true
					events = append(events, "chown:"+path)
					return nil
				},
				func(path string, mode os.FileMode) error {
					if !chowned[path] {
						t.Fatalf("chmod %s before taking ownership", path)
					}
					chmods[path] = mode
					events = append(events, "chmod:"+path)
					return nil
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if chowned[root] != tc.protectsRoot {
				t.Fatalf("session root protected = %v, want %v", chowned[root], tc.protectsRoot)
			}
			if tc.protectsRoot {
				if chmods[root] != 0o710 || events[0] != "chown:"+root {
					t.Fatalf("session root must be protected first with mode 710: %v", events)
				}
			}
			want := map[string]os.FileMode{
				workspace: 0o750, nested: 0o750,
				filepath.Join(nested, "group-writable"):     0o644,
				filepath.Join(nested, "private"):            0o640,
				filepath.Join(nested, "private-executable"): 0o750,
			}
			for path := range files {
				if _, ok := want[path]; !ok {
					want[path] = 0 // already read-only for the group: no chmod
				}
			}
			for path, mode := range want {
				if !chowned[path] {
					t.Fatalf("%s was not protected", path)
				}
				if chmods[path] != mode {
					t.Fatalf("chmod for %s = %o, want %o", path, chmods[path], mode)
				}
			}
			if !chowned[link] || chmods[link] != 0 {
				t.Fatalf("symlink must be re-owned without chmod: chowned=%v mode=%o", chowned[link], chmods[link])
			}
		})
	}
}

func TestProtectReadOnlySessionWorkspaceNonRootOwnIdentity(t *testing.T) {
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		t.Skip("non-root behavior")
	}
	root := t.TempDir()
	paths := SessionPaths{Root: root, Workspace: filepath.Join(root, "workspace")}
	if err := ProtectReadOnlySessionWorkspace(paths, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := ProtectReadOnlySessionWorkspace(paths, uid+1, gid+1); err == nil {
		t.Fatal("non-root supervisor protected a workspace for another identity")
	}
}

func TestPrepareReadOnlyWorkspaceModes(t *testing.T) {
	workspace := t.TempDir()
	nested := filepath.Join(workspace, ".claude")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]os.FileMode{
		filepath.Join(workspace, "private"):           {0o600, 0o640},
		filepath.Join(workspace, "private-tool"):      {0o700, 0o750},
		filepath.Join(nested, "settings.json"):        {0o600, 0o640},
		filepath.Join(workspace, "world-writable"):    {0o666, 0o644},
		filepath.Join(workspace, "already-read-only"): {0o644, 0o644},
	}
	for path, modes := range want {
		if err := os.WriteFile(path, []byte("x"), modes[0]); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, modes[0]); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("private", filepath.Join(workspace, "link")); err != nil {
		t.Fatal(err)
	}
	if err := PrepareReadOnlyWorkspaceModes(workspace); err != nil {
		t.Fatal(err)
	}
	for path, modes := range want {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != modes[1] {
			t.Fatalf("mode for %s = %o, want %o", path, info.Mode().Perm(), modes[1])
		}
	}
	if info, err := os.Lstat(nested); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode changed: %v, %v", info, err)
	}
	if err := PrepareReadOnlyWorkspaceModes("relative"); err == nil {
		t.Fatal("relative workspace accepted")
	}
}
