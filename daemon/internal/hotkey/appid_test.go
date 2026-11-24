// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package hotkey

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The app id the daemon names itself by is the .desktop file the installers and the packages put down,
// and install.sh writes that file's every byte, since it is fetched alone and cannot copy it: the portal
// knows Norite only by a file of that name, and a file that drifted from the packages' would be two
// descriptions of one program.
func TestTheAppIDIsTheDesktopFileEveryInstallPutsDown(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	want, err := os.ReadFile(filepath.Join(root, "packaging", AppID+".desktop"))
	require.NoError(t, err, "packaging/ has no .desktop file for %s", AppID)
	script, err := os.ReadFile(filepath.Join(root, "scripts", "install.sh"))
	require.NoError(t, err)
	_, rest, ok := strings.Cut(string(script), "<<'DESKTOP'\n")
	require.True(t, ok, "install.sh writes no desktop entry")
	body, _, ok := strings.Cut(rest, "\nDESKTOP\n")
	require.True(t, ok)
	require.Equal(t, string(want), body+"\n", "install.sh's desktop entry is not packaging/'s")
	require.Contains(t, string(script), "app_id="+AppID+"\n")

	release, err := os.ReadFile(filepath.Join(root, ".goreleaser.yaml"))
	require.NoError(t, err)
	installed := "/usr/share/applications/" + AppID + ".desktop"
	// The deb, rpm and pacman packages, the AUR's norite-bin from the archive, and its norite from source.
	require.Contains(t, string(release), "dst: "+installed)
	require.Contains(t, string(release), "dst: "+AppID+".desktop")
	require.Equal(t, 2, strings.Count(string(release), `"${pkgdir}`+installed+`"`))
}
