//go:build windows

package aeh

import (
	"os/exec"
	"os/user"
	"path/filepath"
	"testing"
)

// Every store entry ends up owned by the current user; an entry owned by another account
// is taken over or refused, never trusted as it is.
func TestSessionStoreOwner(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	privateProject(t, root)
	store, err := sessionStore(root, "p", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := storeSessionFile(store, ".cfg/tok", []byte("v")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Dir(store), store, filepath.Join(store, ".cfg"), filepath.Join(store, ".cfg", "tok")} {
		if owner, err := fileOwner(p); err != nil || owner != me.Uid {
			t.Errorf("%s owned by %s, want %s (%v)", p, owner, me.Uid, err)
		}
	}
	foreign := filepath.Join(store, ".other")
	write(t, store, ".other", "o")
	if out, err := exec.Command("icacls", foreign, "/setowner", "*S-1-5-18").CombinedOutput(); err != nil {
		t.Skipf("cannot give a file another owner here: %v %s", err, out)
	}
	if owner, _ := fileOwner(foreign); owner != "S-1-5-18" {
		t.Skipf("owner is %s after /setowner", owner)
	}
	if err := protectPrivate(foreign, false); err == nil {
		if owner, _ := fileOwner(foreign); owner != me.Uid {
			t.Fatalf("a foreign-owned file was trusted: owner %s", owner)
		}
	}
}
