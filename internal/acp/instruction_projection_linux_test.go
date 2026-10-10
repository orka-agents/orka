package acp

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestProtectedInstructionChildProbe(t *testing.T) {
	path := os.Getenv("ORKA_TEST_INSTRUCTION_PATH")
	if path == "" {
		t.Skip("child-only probe")
	}
	if os.Getenv("ORKA_TEST_OTHER_SESSION") == "1" {
		if _, err := os.ReadFile(path); err == nil {
			t.Fatal("another Session read protected instructions")
		}
		return
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "literal persona" {
		t.Fatalf("intended child cannot read instructions: uid=%d gid=%d error=%v", os.Getuid(), os.Getgid(), err)
	}
	operations := []func() error{
		func() error { return os.WriteFile(path, []byte("replacement"), 0o600) },
		func() error { return os.Chmod(path, 0o600) },
		func() error { return os.Remove(path) },
		func() error { return os.Rename(path, path+".moved") },
		func() error { return os.Rename(filepath.Dir(path), filepath.Dir(path)+".moved") },
		func() error {
			return os.Rename(filepath.Dir(filepath.Dir(path)), filepath.Dir(filepath.Dir(path))+".moved")
		},
	}
	for i, operation := range operations {
		if err := operation(); err == nil {
			t.Fatalf("protected instruction mutation %d succeeded", i)
		}
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "runtime-state"), []byte("state"), 0o600); err != nil {
		t.Fatal("runtime state is not writable")
	}
}

func TestProtectedInstructionProjection(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root; run the compiled test in a network-disabled container")
	}
	base, err := os.MkdirTemp(os.TempDir(), "orka-instructions-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if err := os.Chmod(base, 0o711); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{".copilot/copilot-instructions.md", ".orka/agent-instructions.md"} {
		t.Run(filepath.Dir(relative), func(t *testing.T) {
			paths, err := PrepareSessionPaths(base, "instruction-test")
			if err != nil {
				t.Fatal(err)
			}
			if err := FinalizeSessionOwnership(paths.Root, 21001, 21001); err != nil {
				t.Fatal(err)
			}
			if err := ProjectInstructions(paths, InstructionProjection{RelativePath: relative, Content: "literal persona"}, 21001); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(paths.Home, relative)
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			for cycle := range 2 {
				if cycle > 0 {
					if err := ReclaimSessionOwnership(paths.Root); err != nil {
						t.Fatal(err)
					}
					if err := FinalizeSessionOwnership(paths.Root, 21001, 21001); err != nil {
						t.Fatal(err)
					}
					if err := RestoreInstructions(paths, InstructionProjection{RelativePath: relative, Content: "literal persona"}, 21001); err != nil {
						t.Fatal(err)
					}
				}
				for _, uid := range []uint32{21001, 21002} {
					cmd := exec.Command(binary, "-test.run=^TestProtectedInstructionChildProbe$")
					cmd.Env = []string{"ORKA_TEST_INSTRUCTION_PATH=" + path}
					if uid == 21002 {
						cmd.Env = append(cmd.Env, "ORKA_TEST_OTHER_SESSION=1")
					}
					cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: uid, Groups: []uint32{}}}
					if output, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("child permission probe failed: %v\n%s", err, output)
					}
				}
			}
			if err := ProjectInstructions(paths, InstructionProjection{RelativePath: relative, Content: "changed"}, 21001); err == nil {
				t.Fatal("projection replaced existing input")
			}
		})
	}
}
