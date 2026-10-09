// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package daemonctl

import (
	"bytes"
	"os"
	"strconv"
)

// isZombie reports whether the process has exited and is waiting to be reaped, from /proc/<pid>/stat.
// The state is the field after the command's name, which is in parentheses and may itself hold spaces and
// parentheses, so it is found from the last one.
func isZombie(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	end := bytes.LastIndexByte(stat, ')')
	return end >= 0 && end+2 < len(stat) && stat[end+2] == 'Z'
}
