//go:build windows

package keyvault

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// hardenPath rewrites the DACL of path so that only the account running the
// service can touch it.
//
// os.MkdirAll(dir, 0o700) and os.WriteFile(file, key, 0o600) do nothing on
// Windows: the mode argument is ignored and the entries simply inherit the
// parent directory's ACL. Under the default %USERPROFILE% or C:\ layout that
// usually means BUILTIN\Administrators plus SYSTEM, and on a machine where the
// data directory was created by an installer it can mean far more. vault.key is
// the AES key for every provider secret, so "who can read this file" is the
// whole threat model of §10.1 — it cannot be left to inherited permissions.
//
// The DACL is replaced, not merged: DACL_SECURITY_INFORMATION alone keeps
// inherited entries, which is exactly how the file ended up readable by more
// than its owner. PROTECTED_DACL_SECURITY_INFORMATION cuts inheritance first.
// For a directory the single ACE is marked inheritable so files created later
// inside (sealed key blobs under keys/) are covered without a second call.
func hardenPath(path string, isDir bool) error {
	user, err := currentUserSID()
	if err != nil {
		return err
	}
	inheritance := uint32(windows.NO_INHERITANCE)
	if isDir {
		inheritance = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user),
		},
	}}, nil)
	if err != nil {
		return err
	}
	// WRITE_DAC is required to change a DACL; the owner normally has it, but ask
	// for it explicitly so a ACL written by an installer cannot lock us out of
	// our own vault on the next boot.
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil,
	); err != nil {
		return fmt.Errorf("设置 DACL %s: %w", path, err)
	}
	return nil
}

// currentUserSID returns the SID of the account this process runs as.
func currentUserSID() (*windows.SID, error) {
	tok, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, fmt.Errorf("打开进程令牌: %w", err)
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("读取令牌用户: %w", err)
	}
	if u.User.Sid == nil {
		return nil, fmt.Errorf("令牌里没有用户 SID")
	}
	return u.User.Sid, nil
}

// warnHardening writes the reason a path could not be locked down. Startup
// continues: a vault on a filesystem without ACLs (FAT32 USB stick) still works,
// it is just as protected as the stick is, and refusing to boot would turn a
// degraded setup into a broken one. The operator has to be told, though —
// silently falling back to inherited permissions is how this stayed hidden.
func warnHardening(path string, err error) {
	fmt.Fprintf(os.Stderr,
		"keyvault: 警告：%s 的权限未能收紧（%v）。%s 仍依赖目录权限，"+
			"同机其他账户可能读到机器密钥并解开全部服务商 Key\n",
		path, err, filepath.Base(path))
}
