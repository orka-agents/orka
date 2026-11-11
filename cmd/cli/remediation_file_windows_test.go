package main

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"github.com/orka-agents/orka/internal/remediation/contract"
)

func remediationWindowsTestUser(t *testing.T) *windows.SID {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(t, err)
	return user.User.Sid
}

func setRemediationWindowsTestACL(t *testing.T, path, rules string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString("D:P" + rules)
	require.NoError(t, err)
	acl, _, err := sd.DACL()
	require.NoError(t, err)
	require.NoError(t, windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil))
}

func TestRemediationWindowsAuthenticationUsesPrivateACLs(t *testing.T) {
	for _, mode := range []string{"token-file", "saved-login"} {
		for _, broad := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/broad=%t", mode, broad), func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("USERPROFILE", home)
				t.Setenv("HOME", home)
				token := "synthetic-" + rand.Text()
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.Header.Get("Authorization") != "Bearer "+token {
						t.Error("authentication token did not match the private input")
					}
					remediationReply(w, http.StatusOK, "request-one", contract.PhaseCancelled,
						contract.StageComplete, contract.OutcomeCancelled, false)
				}))
				defer server.Close()
				file := filepath.Join(home, "token")
				content := token
				args := []string{"status", "request-one", "--server", server.URL, "--namespace", "testing"}
				if mode == "saved-login" {
					file = configPath()
					require.NoError(t, os.MkdirAll(filepath.Dir(file), 0700))
					content = fmt.Sprintf("server: %s\nnamespace: testing\ntoken: %s\n", server.URL, token)
				} else {
					args = append(args, "--token-file", file)
				}
				require.NoError(t, os.WriteFile(file, []byte(content), 0600))
				rules := "(A;;FA;;;" + remediationWindowsTestUser(t).String() + ")(A;;FA;;;SY)(A;;FA;;;BA)"
				if broad {
					rules += "(A;;GR;;;WD)"
				}
				setRemediationWindowsTestACL(t, file, rules)
				_, _, err := remediationCLI(t, args...)
				if broad {
					require.Error(t, err)
					require.Zero(t, calls.Load())
				} else {
					require.NoError(t, err)
					require.EqualValues(t, 1, calls.Load())
				}
			})
		}
	}
}

func TestRemediationWindowsSecurityDescriptorRejectsUnsafeAuthority(t *testing.T) {
	user := remediationWindowsTestUser(t)
	for _, test := range []struct {
		name, descriptor string
		allowed          bool
	}{
		{"owner only", "O:" + user.String() + "D:P(A;;FR;;;" + user.String() + ")", true},
		{"administrator owner", "O:BAD:P(A;;FA;;;BA)(A;;FR;;;" + user.String() + ")", true},
		{"untrusted owner", "O:BUD:P(A;;FR;;;" + user.String() + ")", false},
		{"null ACL", "O:" + user.String() + "D:NO_ACCESS_CONTROL", false},
		{"world read", "O:" + user.String() + "D:P(A;;FR;;;WD)", false},
		{"world write", "O:" + user.String() + "D:P(A;;FW;;;WD)", false},
		{"world ACL changes", "O:" + user.String() + "D:P(A;;WD;;;WD)", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(test.descriptor)
			require.NoError(t, err)
			err = validateRemediationWindowsFileSecurity(sd, user)
			if test.allowed {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestRemediationWindowsPatchRequiresPrivateInheritedACL(t *testing.T) {
	user := remediationWindowsTestUser(t)
	for _, broad := range []bool{false, true} {
		t.Run(fmt.Sprintf("broad=%t", broad), func(t *testing.T) {
			directory := t.TempDir()
			rules := "(A;OICI;FA;;;" + user.String() + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
			if broad {
				rules += "(A;OICI;GR;;;WD)"
			}
			setRemediationWindowsTestACL(t, directory, rules)
			path := filepath.Join(directory, "candidate.patch")
			err := writeRemediationPatch(path, []byte(remediationSyntheticPatch))
			if broad {
				require.Error(t, err)
				_, statErr := os.Stat(path)
				require.ErrorIs(t, statErr, os.ErrNotExist)
			} else {
				require.NoError(t, err)
				content, err := readRemediationFile(path, remediationMaxPatchBytes, true)
				require.NoError(t, err)
				require.Equal(t, remediationSyntheticPatch, string(content))
			}
		})
	}
}
