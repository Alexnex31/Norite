// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package instanceinit

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Document.Write removes the file on any error from the write. A directory that cannot be flushed is an
// error from the shared writer and not a failed write: removing the file then would delete a complete
// configuration, the only copy of the database credentials, for a durability shortfall.
func TestAWriteWhoseDirectoryCannotBeFlushedKeepsTheFile(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory this user can write to and not read")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "instance.toml")
	require.NoError(t, os.Chmod(dir, 0o300))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	require.NoError(t, writeAtomically(path, "listen_addr = \":8080\"\n"))

	require.NoError(t, os.Chmod(dir, 0o700))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "listen_addr = \":8080\"\n", string(got))
}
