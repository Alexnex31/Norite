package daemonctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

const unitFileName = ServiceName + ".service"

// systemdUser drives a systemd *user* unit.
//
// A user unit, never a system one: it runs as the invoking account with that account's session keyring and
// home directory, and installing it needs no root. A system unit would need elevation to install, would run
// as the wrong user, and would have no access to the keychain holding that user's tokens.
type systemdUser struct{ run Runner }

// unitTemplate is the service definition.
//
// Hardening is deliberately limited to directives that work unprivileged in a *user* unit on any systemd
// recent enough to be in a supported distribution. The tempting ones — ProtectSystem, PrivateTmp — either
// need privileges or need unprivileged user namespaces, and a unit that refuses to start on a slightly
// older systemd is a far worse outcome than one sandbox layer short.
var unitTemplate = template.Must(template.New("unit").Parse(`[Unit]
Description=Norite background daemon
Documentation=https://github.com/Alexnex31/Norite
# The daemon holds a persistent WebSocket to the instance. Ordering after the network is a hint, not a
# guarantee, so the daemon still has to tolerate starting with no route — but it avoids a pointless failed
# connection on every single boot.
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart={{ .ExecStart }}
{{ if .StateHome }}
# Captured from the environment at install time, because the systemd user manager does not read your shell
# profile. Without this, a user who exports XDG_STATE_HOME in ~/.bashrc gets one state directory from a
# hand-started daemon and a different one from the service — so the two take *different* single-instance
# locks, both start, and the one-daemon-per-user invariant is broken with no error anywhere.
Environment=XDG_STATE_HOME={{ .StateHome }}
{{ end }}
# Restart on a crash, but not when the daemon exits 0 — a clean stop is a decision, and restarting after
# one would make ` + "`systemctl --user stop`" + ` impossible.
Restart=on-failure
RestartSec=5s

# Exit code 3 is "another daemon is already running for this user". That is the one failure that must never
# be retried: the condition is by definition already satisfied, and retrying produces a restart loop that
# achieves nothing.
RestartPreventExitStatus=3

KillSignal=SIGTERM
# Comfortably longer than the daemon's own shutdown path needs, so the daemon decides how a stop ends
# rather than being SIGKILLed part-way through one.
TimeoutStopSec=30

NoNewPrivileges=true
RestrictSUIDSGID=true
RestrictRealtime=true
LockPersonality=true

[Install]
WantedBy=default.target
`))

func (s *systemdUser) DefinitionPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locating the user's home directory: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user", unitFileName), nil
}

// StartsOnInstall is false: Install uses `enable`, not `enable --now`, on purpose.
func (s *systemdUser) StartsOnInstall() bool { return false }

func (s *systemdUser) LogHint() string {
	return "journalctl --user -u " + unitFileName + " -f"
}

func (s *systemdUser) Install(ctx context.Context, daemonBinary string) error {
	path, err := s.DefinitionPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}

	// Only an absolute value is carried over: a relative XDG_STATE_HOME is invalid per the spec and is
	// ignored by the daemon too, so baking one in would pin the unit to a setting that does nothing.
	stateHome := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(stateHome) {
		stateHome = ""
	} else {
		stateHome = systemdEscape(stateHome)
	}

	var unit strings.Builder
	err = unitTemplate.Execute(&unit, struct{ ExecStart, StateHome string }{
		ExecStart: systemdEscape(daemonBinary),
		StateHome: stateHome,
	})
	if err != nil {
		return fmt.Errorf("rendering the unit file: %w", err)
	}

	// 0644, not 0600: systemd reads user units as the user, so the mode is not a security boundary, and a
	// file the user cannot read with a plain `cat` is an obstacle when something goes wrong. Nothing secret
	// goes in it — the path of a binary is not a credential.
	if err := os.WriteFile(path, []byte(unit.String()), 0o644); err != nil { //nolint:gosec // see above
		return fmt.Errorf("writing %s: %w", path, err)
	}

	if _, err := mustSucceed(ctx, s.run, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	// enable, not `enable --now`: install and start are separate verbs in this CLI, and an install that
	// silently also started the daemon would make `norite daemon install && norite daemon start` report a
	// confusing already-running state.
	if _, err := mustSucceed(ctx, s.run, "systemctl", "--user", "enable", unitFileName); err != nil {
		return err
	}
	return nil
}

func (s *systemdUser) Uninstall(ctx context.Context) error {
	path, err := s.DefinitionPath()
	if err != nil {
		return err
	}

	// Best-effort stop and disable before removing the file. Both are allowed to fail: the point of
	// uninstall is that the service is gone afterwards, and refusing to remove a unit file because
	// systemctl could not stop an already-dead unit would leave the user stuck with no way forward.
	_, _ = s.run.Run(ctx, "systemctl", "--user", "stop", unitFileName)
	_, _ = s.run.Run(ctx, "systemctl", "--user", "disable", unitFileName)

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", path, err)
	}

	if _, err := mustSucceed(ctx, s.run, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	return nil
}

func (s *systemdUser) Start(ctx context.Context) error {
	if err := s.requireInstalled(); err != nil {
		return err
	}
	// `start` on an already-running unit is a success in systemd, which is the idempotence this interface
	// promises — no need to check first.
	_, err := mustSucceed(ctx, s.run, "systemctl", "--user", "start", unitFileName)
	return err
}

func (s *systemdUser) Stop(ctx context.Context) error {
	if err := s.requireInstalled(); err != nil {
		return err
	}
	_, err := mustSucceed(ctx, s.run, "systemctl", "--user", "stop", unitFileName)
	return err
}

func (s *systemdUser) Status(ctx context.Context) (State, error) {
	path, err := s.DefinitionPath()
	if err != nil {
		return State{}, err
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, nil
		}
		return State{}, fmt.Errorf("checking %s: %w", path, err)
	}

	// is-active answers with a word on stdout and an exit code, and reports "inactive" with exit 3. That is
	// an answer, not a failure, so this deliberately does not go through mustSucceed.
	res, err := s.run.Run(ctx, "systemctl", "--user", "is-active", unitFileName)
	if err != nil {
		return State{}, err
	}
	detail := firstNonEmpty(res.Stdout, res.Stderr, "unknown")
	return State{Installed: true, Running: res.ExitCode == 0, Detail: detail}, nil
}

func (s *systemdUser) requireInstalled() error {
	path, err := s.DefinitionPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return ErrNotInstalled
	}
	return nil
}

// systemdEscape quotes a path for use in ExecStart.
//
// systemd splits ExecStart on whitespace and applies its own unescaping, so an unquoted path containing a
// space silently becomes a command plus an argument. Home directories with spaces in them are ordinary on
// macOS and Windows and not unheard of on Linux.
// Quoting alone is not enough for `$` and `%`: systemd expands variables and specifiers *inside* double
// quotes too, so an unescaped `/opt/build$rev/...` becomes `/opt/build/...` — an unknown variable expands to
// nothing — and the unit fails at every boot naming a path the operator never wrote. Both are escaped by
// doubling.
func systemdEscape(path string) string {
	if !strings.ContainsAny(path, " \t\"'\\$%") {
		return path
	}
	replaced := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`, `%`, `%%`).Replace(path)
	return `"` + replaced + `"`
}
