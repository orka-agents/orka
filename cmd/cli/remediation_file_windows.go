package main

import (
	"errors"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

func checkRemediationPrivateFile(file *os.File, _ os.FileInfo) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return errors.New("could not determine the private file's authorized user")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return errors.New("could not inspect private file ownership and access permissions")
	}
	defer runtime.KeepAlive(sd)
	return validateRemediationWindowsFileSecurity(sd, user.User.Sid)
}

func validateRemediationWindowsFileSecurity(sd *windows.SECURITY_DESCRIPTOR, user *windows.SID) error {
	if sd == nil || !sd.IsValid() || user == nil || !user.IsValid() {
		return errors.New("private file security descriptor is invalid")
	}
	trusted := func(sid *windows.SID) bool {
		return sid != nil && sid.IsValid() &&
			(sid.Equals(user) || sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid))
	}
	owner, _, err := sd.Owner()
	if err != nil || !trusted(owner) {
		return errors.New("private file must be owned by the current user, SYSTEM, or Administrators")
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return errors.New("private file must have a restrictive access-control list")
	}
	for index := range uint32(acl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, index, &ace); err != nil || ace == nil {
			return errors.New("could not inspect private file access-control entry")
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || uintptr(ace.Header.AceSize) < unsafe.Sizeof(*ace) {
			return errors.New("private file has an unsupported access-control entry")
		}
		// Windows stores the variable-length SID inline after the ACE mask.
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Mask != 0 && !trusted(sid) {
			return errors.New("private file access must be limited to the current user, SYSTEM, and Administrators")
		}
	}
	return nil
}
