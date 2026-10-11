// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package installreceipt

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestMain points the state directory at a temporary one: Record and ForgetIfNames resolve the current
// user's, and a test must never reach the real one.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "norite-receipt-")
	if err != nil {
		panic(err)
	}
	for _, name := range []string{"HOME", "XDG_STATE_HOME", "LOCALAPPDATA", "APPDATA"} {
		_ = os.Setenv(name, filepath.Join(dir, name))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestAReceiptIsReadBackAsWritten(t *testing.T) {
	state := t.TempDir()
	if _, err := Read(state); !errors.Is(err, ErrNone) {
		t.Fatalf("no receipt: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "bin")
	if err := Write(state, Receipt{BinDir: bin + string(filepath.Separator) + ".", By: "install.sh"}); err != nil {
		t.Fatal(err)
	}
	got, err := Read(state)
	if err != nil || got != (Receipt{BinDir: bin, By: "install.sh"}) {
		t.Errorf("read %+v, %v", got, err)
	}
	if err := Write(state, Receipt{BinDir: "relative/bin"}); err == nil {
		t.Error("a receipt naming a relative directory was written")
	}
	for _, body := range []string{"", "not json", `{"bin_dir":"relative"}`, `{"bin_dir":7}`} {
		if err := os.WriteFile(filepath.Join(state, FileName), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(state); err == nil || errors.Is(err, ErrNone) {
			t.Errorf("receipt %q: %v", body, err)
		}
	}
	if err := Remove(state); err != nil {
		t.Fatal(err)
	}
	if err := Remove(state); err != nil {
		t.Errorf("removing one that is not there: %v", err)
	}
}

// An installer run against another directory leaves the account's receipt alone.
func TestOnlyAReceiptForThatDirectoryIsForgotten(t *testing.T) {
	mine, other := filepath.Join(t.TempDir(), "bin"), filepath.Join(t.TempDir(), "opt")
	if err := ForgetIfNames(mine); err != nil {
		t.Fatalf("with no receipt: %v", err)
	}
	if err := Record(Receipt{BinDir: mine, By: "install.sh"}); err != nil {
		t.Fatal(err)
	}
	state := filepath.Dir(mustReceiptPath(t))
	if err := ForgetIfNames(other); err != nil {
		t.Fatal(err)
	}
	if got, err := Read(state); err != nil || got.BinDir != mine {
		t.Fatalf("another directory's uninstall forgot this install's receipt: %+v, %v", got, err)
	}
	if err := ForgetIfNames(mine + string(filepath.Separator)); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(state); !errors.Is(err, ErrNone) {
		t.Errorf("its own directory's uninstall left the receipt: %v", err)
	}
}

// mustReceiptPath finds the receipt Record wrote, under the temporary roots TestMain set.
func mustReceiptPath(t *testing.T) string {
	t.Helper()
	var found string
	_ = filepath.WalkDir(filepath.Dir(os.Getenv("HOME")), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == FileName {
			found = path
		}
		return nil
	})
	if found == "" {
		t.Fatal("Record wrote no receipt under the temporary home")
	}
	return found
}
