//go:build windows

package keyvault

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// The Windows gap this covers: os.MkdirAll(dir, 0o700) and os.WriteFile(k, 0o600)
// do not apply any ACL, so vault.key inherited whatever the parent directory
// granted — usually Administrators and SYSTEM, sometimes more. vault.key is the
// AES key for every provider secret, so "who may read it" *is* the threat model.
//
// The assertions go through SDDL rather than counting ACEs: Windows splits an
// inheritable entry on a container into "this folder" + "inherit-only for
// children", so the count is 1 for a file and 2 for a directory while the set of
// principals is what actually matters.

// sddl returns the security descriptor in SDDL form, e.g.
// "O:S-1-5-21-…G:S-1-5-21-…D:P(A;OICI;FA;;;S-1-5-21-…-500)".
func sddl(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		// Not 0xff: asking for the SACL requires SeSecurityPrivilege, which a
		// normal process does not hold (ERROR_PRIVILEGE_NOT_HELD). Owner, group
		// and DACL is the whole picture here anyway.
		windows.OWNER_SECURITY_INFORMATION|
			windows.GROUP_SECURITY_INFORMATION|
			windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read security info for %s: %v", path, err)
	}
	s := sd.String()
	if s == "" {
		t.Fatalf("could not render the SDDL for %s", path)
	}
	return s
}

// testUserSID is the SID string of the account running the tests. (The package's
// own currentUserSID returns a *windows.SID and is exercised through hardenPath.)
func testUserSID(t *testing.T) string {
	t.Helper()
	sid, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}
	return sid.String()
}

// assertOwnerOnly fails unless the DACL is protected and every entry names only
// the current user.
func assertOwnerOnly(t *testing.T, path string) {
	t.Helper()
	s := sddl(t, path)
	sid := testUserSID(t)

	// "D:P" is the protected-DACL flag: without it, entries inherited from the
	// parent folder silently re-widen access the next time it changes.
	if !strings.Contains(s, "D:P") {
		t.Errorf("%s: DACL is not protected — inherited entries can still flow in.\n  SDDL: %s", path, s)
	}

	// Every ACE's trustee must be us. SDDL renders the machine's local
	// Administrator account as the alias "LA" instead of its full S-1-5-21-…-500,
	// so both spellings are accepted — and nothing else is.
	trustees := sddlTrustees.FindAllStringSubmatch(s, -1)
	if len(trustees) == 0 {
		t.Fatalf("%s: DACL has no entries at all (that means everyone).\n  SDDL: %s", path, s)
	}
	for _, m := range trustees {
		if trustee := m[1]; trustee != sid && trustee != "LA" {
			t.Errorf("%s: DACL grants %s, which is not the service account.\n  SDDL: %s", path, trustee, s)
		}
	}
}

// sddlTrustees matches the trustee field of each ACE: "(A;;FA;;;LA)" -> LA.
var sddlTrustees = regexp.MustCompile(`;;;([A-Za-z0-9-]+)\)`)

func TestHardenFileReplacesInheritedDACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.bin")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := hardenPath(path, false); err != nil {
		t.Fatalf("hardenPath: %v", err)
	}
	assertOwnerOnly(t, path)
}

func TestHardenDirIsInheritable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := hardenPath(dir, true); err != nil {
		t.Fatalf("hardenPath: %v", err)
	}
	assertOwnerOnly(t, dir)

	// The directory entry must be inheritable, otherwise sealed key blobs
	// written into it later get no DACL of their own and fall back to the
	// parent's.
	if s := sddl(t, dir); !strings.Contains(s, "OICI") && !strings.Contains(s, "CI") {
		t.Errorf("directory ACE is not inheritable (no OI/CI flags).\n  SDDL: %s", s)
	}
}

// End to end through the vault itself: after New, both the directory and the
// machine key are owner-only, and an existing key is re-hardened — the upgrade
// path, since a key created by an older build still carries the inherited ACL.
func TestNewLocksDownVaultAndKey(t *testing.T) {
	dataDir := t.TempDir()
	if _, err := New(dataDir); err != nil {
		t.Fatalf("New: %v", err)
	}

	vaultDir := filepath.Join(dataDir, "vault")
	keyPath := filepath.Join(vaultDir, keyFile)
	assertOwnerOnly(t, vaultDir)
	assertOwnerOnly(t, keyPath)

	// Hardening must not touch the bytes, only the permissions.
	before, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	v2, err := New(dataDir)
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	after, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("re-read key: %v", err)
	}
	if string(before) != string(after) {
		t.Error("re-opening the vault rewrote the machine key")
	}
	if v2 == nil || len(v2.key) != 32 {
		t.Error("vault did not load a 32-byte machine key on reopen")
	}
	// A sealed-blob round trip still works after the ACL rewrite, i.e. we have
	// not locked the service out of its own vault.
	blob, err := v2.encrypt([]byte("sk-test"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	plain, err := v2.decrypt(blob)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(plain) != "sk-test" {
		t.Errorf("round trip returned %q", plain)
	}
}
