// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package hotkey

// AppID is Norite's application id on Linux (the author's choice at M35's planning): the name of the
// .desktop file install.sh and the packages install, which an unsandboxed program needs before the
// GlobalShortcuts portal will name it (ADR 0034 §5), and the section the desktop keeps the person's binding
// under. It never changes after a release: the bindings people made would be lost.
const AppID = "io.github.alexnex31.Norite"
