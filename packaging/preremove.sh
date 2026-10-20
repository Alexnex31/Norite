#!/bin/sh
# Runs before the norite package is removed, as root, by dpkg, rpm or pacman (ADR 0035 §3).
#
# Root cannot reach a user's systemd manager, so this removes nobody's daemon service. It says how, and only
# on a removal: dpkg passes "upgrade" on an upgrade, rpm passes how many copies remain (1 or more on an
# upgrade), and pacman runs it on a removal only, passing the old version. A version such as 1.0.0-1 is not
# an integer, so the test below never mistakes it for rpm's count.
case "$1" in
upgrade | failed-upgrade) exit 0 ;;
esac
if [ "$1" -ge 1 ] 2>/dev/null; then
	exit 0
fi

cat <<'MESSAGE'
norite: removing the package leaves the daemon's service of every account that ran `norite daemon install`.
It no longer starts, and stays listed. To remove it, run as that account:

    systemctl --user disable --now norite-daemon.service
    rm "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/norite-daemon.service"
    systemctl --user daemon-reload

MESSAGE
exit 0
