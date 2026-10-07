// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package credentials

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A directory that can be written to and not opened cannot be flushed, so the shared writer reports the
// write as not durable. The file was replaced all the same. Reporting that as a failure would leave the
// session presenting a token the store no longer holds, which reuse detection answers by ending the
// sign-in.
func TestAWriteWhoseDirectoryCannotBeFlushedIsStillAWrite(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory this user can write to and not read")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
	require.NoError(t, os.Chmod(dir, 0o300))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	require.NoError(t, writeFileAtomically(path, []byte("new")))

	require.NoError(t, os.Chmod(dir, 0o700))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))
}
