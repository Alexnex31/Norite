#!/bin/sh
#
# Set up a Norite instance on this machine, for the current user, on Linux or macOS. It never asks for root.
#
#   curl -fsSL https://raw.githubusercontent.com/Alexnex31/Norite/main/scripts/install-server.sh |
#       sh -s -- --url http://192.168.1.20:8080 --docker-postgres
#   ./scripts/install-server.sh --url https://chat.example.com --behind-proxy --docker-postgres
#
# What it does, in order: gets norite-server and norite through install.sh, which is where downloading and
# checking live, so there is one copy of that; optionally starts Postgres in Docker; writes instance.toml
# with `norite instance init`; registers the server as a systemd user service that survives logout; waits
# for it to answer; and creates the administrator. Run again on a configured instance, it upgrades the
# programs and restarts the service, and touches nothing else.
#
# What it does not do is the part only you can: making --url reach this machine. docs/trying-the-alpha.md
# covers the three ways, a shared network, a reverse proxy, or a tunnel.
#
# POSIX sh, and wholly inside main(), for the reasons install.sh gives.

set -eu
umask 077

REPO="Alexnex31/Norite"

say()  { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

have() { command -v "$1" >/dev/null 2>&1; }
have_tty() { (: </dev/tty) 2>/dev/null; }

usage() {
    cat <<'EOF'
Set up a Norite instance on this machine.

  install-server.sh --url URL [options]

The instance:
  --url URL             the address people will reach it on, e.g. http://192.168.1.20:8080 (required)
  --behind-proxy        a reverse proxy or tunnel on this machine handles HTTPS: listen on 127.0.0.1 only,
                        and believe the visitor address it forwards
  --port PORT           the port the server listens on (default: 8080)
  --registration MODE   open or invite (default: invite when the URL is https, otherwise open)
  --dir DIR             where instance.toml lives (default: ~/norite-instance)

Its database:
  --docker-postgres     start Postgres 16 in Docker for it, with a generated password
  --postgres-port PORT  with --docker-postgres, the local port it is published on (default: 5432)
                        Without --docker-postgres, the setup asks how to reach a Postgres you already have.

The programs (passed to install.sh):
  --version TAG         install this release (default: the newest)
  --from-source         build the checkout this script is in (the default when it is in one)
  --release             download a release even when run from a checkout
  --bin-dir DIR         install into DIR (default: ~/.local/bin)

Doing less:
  --no-service          do not register a service; print the command that runs the server instead
  --no-bootstrap        do not create the administrator
  --admin-username NAME, --admin-email EMAIL
                        create the administrator without asking; its password comes from NORITE_ADMIN_PASSWORD
  -h, --help            this text
EOF
}

get() { # get URL → stdout, quietly; fails when the URL does
    if have curl; then curl -fsS "$1" 2>/dev/null
    else wget -q -O - "$1" 2>/dev/null
    fi
}

# run_installer hands the programs to install.sh: the one beside this script, or the published one.
run_installer() {
    installer=""
    case "$0" in
        */*)
            d=$(cd "$(dirname "$0")" 2>/dev/null && pwd) || d=""
            [ -n "$d" ] && [ -f "$d/install.sh" ] && installer="$d/install.sh"
            ;;
    esac
    if [ -z "$installer" ]; then
        # Its own name: sh has no local variables, and `url` is the instance's address.
        script_url=${NORITE_INSTALL_SCRIPT_URL:-https://raw.githubusercontent.com/$REPO/main/scripts/install.sh}
        installer="$tmp/install.sh"
        get "$script_url" >"$installer" || die "could not download install.sh from $script_url"
    fi
    # shellcheck disable=SC2086  # $mode_args is zero or more whole flags, split on purpose
    sh "$installer" --with-server --no-daemon --no-account --no-summary --bin-dir "$bin_dir" $mode_args
}

start_postgres() {
    have docker || die "--docker-postgres needs Docker; without it, omit the flag and point the setup at your own Postgres"
    name=${NORITE_PG_CONTAINER:-norite-postgres}
    docker inspect "$name" >/dev/null 2>&1 &&
        die "a container named $name already exists. Remove it, or omit --docker-postgres and answer the setup's database questions"
    # A volume left by an earlier install keeps that install's password, and Postgres ignores a new one.
    docker volume inspect "$name-data" >/dev/null 2>&1 &&
        die "a Docker volume named $name-data already exists, holding an earlier database. Remove it with 'docker volume rm $name-data' to start clean"

    step "Starting Postgres 16 in Docker, as $name"
    db_password=$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32)
    # Through a file rather than -e, so the password is in no process list.
    printf 'POSTGRES_USER=norite\nPOSTGRES_PASSWORD=%s\nPOSTGRES_DB=norite\n' "$db_password" >"$tmp/pg.env"
    docker run -d --name "$name" --restart unless-stopped --env-file "$tmp/pg.env" \
        -p "127.0.0.1:$pg_port:5432" -v "$name-data:/var/lib/postgresql/data" postgres:16-alpine >/dev/null
    # Over TCP on purpose: the image's first start runs a temporary server on the socket alone, and
    # answering there is not being ready.
    i=0
    until docker exec "$name" pg_isready -q -h 127.0.0.1 -U norite -d norite >/dev/null 2>&1; do
        i=$((i + 1))
        [ "$i" -lt 60 ] || die "Postgres did not become ready; see: docker logs $name"
        sleep 1
    done
    say "Postgres is ready on 127.0.0.1:$pg_port, and starts with Docker."
}

configure() {
    step "Configuring the instance in $dir"
    mkdir -p "$dir"
    if [ "$behind_proxy" = 1 ]; then listen="127.0.0.1:$port"; else listen=":$port"; fi
    if [ -z "$registration" ] && [ "$docker_postgres" = 1 ]; then
        case "$url" in
            https://*) registration=invite ;;
            *)         registration=open ;;
        esac
    fi

    set -- instance init -o "$config" --public-base-url "$url" --listen-addr "$listen"
    [ -n "$registration" ] && set -- "$@" --registration "$registration"
    if [ "$docker_postgres" = 1 ]; then
        NORITE_DB_PASSWORD=$db_password "$norite" "$@" --non-interactive \
            --db-host 127.0.0.1 --db-port "$pg_port" --db-name norite --db-user norite --db-sslmode disable \
            >"$tmp/init.log" 2>&1 || { cat "$tmp/init.log" >&2; die "writing the configuration failed"; }
    else
        have_tty || die "there is no terminal to answer the database questions on; use --docker-postgres, or run this from a terminal"
        "$norite" "$@" </dev/tty || die "the configuration was not written"
    fi

    if [ "$behind_proxy" = 1 ]; then
        # Without it every visitor arrives as the proxy's address, and the rate limits count them as one.
        awk '{ print } /^public_base_url = / { print ""; print "# Set by install-server.sh --behind-proxy: the proxy on this machine forwards each visitor'"'"'s address."; print "trust_proxy_headers = true" }' \
            "$config" >"$config.new"
        mv -f "$config.new" "$config"
    fi
    say "Wrote $config. It holds the instance's signing key and the database password: keep it private,"
    say "and keep a copy somewhere safe."
}

# service_usable: a systemd user manager this session can talk to.
service_usable() {
    [ "$os" = linux ] && have systemctl && systemctl --user show-environment >/dev/null 2>&1
}

install_service() {
    # The paths go into a unit file, where spaces, quotes and percent signs each mean something.
    case "$bin_dir$dir" in
        *[!A-Za-z0-9/._-]*)
            warn "the install paths contain characters a service definition would misread, so no service was registered"
            return 1
            ;;
    esac
    step "Registering the server as a service ($unit)"
    unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
    mkdir -p "$unit_dir"
    cat >"$unit_dir/$unit.service" <<EOF
[Unit]
Description=Norite server

[Service]
ExecStart=$bin_dir/norite-server -config $config
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
EOF
    chmod 0644 "$unit_dir/$unit.service"
    # Each checked by hand: this function runs as an `if` condition, where `set -e` does not apply.
    systemctl --user daemon-reload || return 1
    systemctl --user enable "$unit.service" >/dev/null 2>&1 || return 1
    systemctl --user restart "$unit.service" || return 1
    # A user service stops when its user logs out, unless the account lingers.
    if loginctl enable-linger "$(id -un)" >/dev/null 2>&1; then
        say "It starts at boot and stays up after you log out."
    else
        warn "could not enable lingering, so the server runs only while you are logged in."
        say "To keep it up after logout: sudo loginctl enable-linger $(id -un)"
    fi
}

wait_healthy() {
    i=0
    until [ "$(get "http://127.0.0.1:$port/api/v1/healthz" || true)" = '{"status":"ok"}' ]; do
        i=$((i + 1))
        if [ "$i" -ge 60 ]; then
            warn "the server did not answer on port $port within a minute."
            say "Its log: journalctl --user -u $unit -n 30"
            return 1
        fi
        sleep 1
    done
    say "The server answers on port $port."
}

bootstrap() {
    step "The administrator account"
    # Through the local address, so it works before a proxy or a tunnel is in front of the server.
    set -- instance bootstrap --config "$config" --instance "http://127.0.0.1:$port"
    if [ -n "$admin_username" ] && [ -n "$admin_email" ] && [ -n "${NORITE_ADMIN_PASSWORD:-}" ]; then
        "$norite" "$@" --username "$admin_username" --email "$admin_email"
    elif have_tty; then
        "$norite" "$@" </dev/tty
    else
        say "No terminal to ask on. Create it with:"
        say "  norite instance bootstrap --config $config --instance http://127.0.0.1:$port"
    fi
}

main() {
    url="" behind_proxy=0 port="" registration="" dir="$HOME/norite-instance"
    docker_postgres=0 pg_port=5432 bin_dir="$HOME/.local/bin" mode_args=""
    service=1 do_bootstrap=1 admin_username="" admin_email=""
    unit=${NORITE_SERVER_UNIT:-norite-server}

    while [ $# -gt 0 ]; do
        case "$1" in
            --url)             [ $# -ge 2 ] || die "--url needs an address"; url=$2; shift ;;
            --behind-proxy)    behind_proxy=1 ;;
            --port)            [ $# -ge 2 ] || die "--port needs a number"; port=$2; shift ;;
            --registration)    [ $# -ge 2 ] || die "--registration needs open or invite"; registration=$2; shift ;;
            --dir)             [ $# -ge 2 ] || die "--dir needs a directory"; dir=$2; shift ;;
            --docker-postgres) docker_postgres=1 ;;
            --postgres-port)   [ $# -ge 2 ] || die "--postgres-port needs a number"; pg_port=$2; shift ;;
            --version)         [ $# -ge 2 ] || die "--version needs a tag"; mode_args="$mode_args --version $2"; shift ;;
            --from-source)     mode_args="$mode_args --from-source" ;;
            --release)         mode_args="$mode_args --release" ;;
            --bin-dir)         [ $# -ge 2 ] || die "--bin-dir needs a directory"; bin_dir=$2; shift ;;
            --no-service)      service=0 ;;
            --no-bootstrap)    do_bootstrap=0 ;;
            --admin-username)  [ $# -ge 2 ] || die "--admin-username needs a name"; admin_username=$2; shift ;;
            --admin-email)     [ $# -ge 2 ] || die "--admin-email needs an address"; admin_email=$2; shift ;;
            -h | --help)       usage; exit 0 ;;
            *)                 usage >&2; die "unknown option: $1" ;;
        esac
        shift
    done

    case "$(uname -s)" in
        Linux)  os=linux ;;
        Darwin) os=darwin ;;
        *)      die "this script is for Linux and macOS" ;;
    esac
    config="$dir/instance.toml"
    case "$port$pg_port" in *[!0-9]*) die "--port and --postgres-port take a number" ;; esac
    case "$registration" in "" | open | invite) ;; *) die "--registration is open or invite" ;; esac
    # A version tag reaches install.sh unquoted, so it is held here to what a tag is made of.
    case "$mode_args" in *[!A-Za-z0-9._+\ -]*) die "that is not a release tag" ;; esac

    fresh=1
    [ -f "$config" ] && fresh=0
    if [ "$fresh" = 0 ]; then
        # An instance already configured says where it is and what it listens on; flags do not override it.
        url=$(sed -n 's/^public_base_url = "\(.*\)"$/\1/p' "$config" | head -n 1)
        port=$(sed -n 's/^listen_addr = ".*:\([0-9][0-9]*\)"$/\1/p' "$config" | head -n 1)
    fi
    [ -n "$port" ] || port=8080
    if [ "$fresh" = 1 ]; then
        [ -n "$url" ] || { usage >&2; die "--url is required: the address people will reach this instance on"; }
        case "$url" in
            http://* | https://*) ;;
            *) die "--url must start with http:// or https://" ;;
        esac
    fi

    tmp=$(mktemp -d 2>/dev/null || mktemp -d -t norite-server)
    trap 'rm -rf "$tmp"' EXIT INT TERM

    run_installer
    norite="$bin_dir/norite"

    if [ "$fresh" = 1 ]; then
        db_password=""
        [ "$docker_postgres" = 1 ] && start_postgres
        configure
    else
        step "Keeping the existing configuration in $config"
        say "Only the programs were updated. Delete that file first to set the instance up from nothing."
    fi

    running=0
    # A server started by hand already holds the port: a service registered now would fail to bind and be
    # restarted every five seconds, while the health check below read the other server's answer as its own.
    if [ "$service" = 1 ] && service_usable && ! systemctl --user is-active --quiet "$unit.service" &&
        get "http://127.0.0.1:$port/api/v1/healthz" >/dev/null; then
        warn "something already answers on port $port, and it is not the $unit service: a server started by hand?"
        say "Stop it and run this again to have the service take over, or pass --no-service."
        service=0
    fi
    if [ "$service" = 1 ] && service_usable && install_service; then
        wait_healthy && running=1
    else
        step "Running the server"
        say "No service was registered. Start the server yourself, and leave it running:"
        say "  $bin_dir/norite-server -config $config"
    fi

    if [ "$fresh" = 1 ] && [ "$do_bootstrap" = 1 ]; then
        if [ "$running" = 1 ]; then
            bootstrap
        else
            say ""
            say "Once it is running, create the administrator:"
            say "  $norite instance bootstrap --config $config --instance http://127.0.0.1:$port"
        fi
    fi

    step "Done"
    [ -n "$url" ] && say "Instance address: $url"
    say "Configuration:    $config"
    [ "$running" = 1 ] && say "Server log:       journalctl --user -u $unit -f"
    say ""
    say "Check it from another machine:  curl ${url:-INSTANCE-URL}/api/v1/healthz"
    say "Let somebody register:          $norite instance invite create --config $config"
    say "Sign in, from any machine:      norite login --instance ${url:-INSTANCE-URL}"
    if [ "$behind_proxy" = 1 ]; then
        say ""
        say "The server listens on 127.0.0.1:$port only. Point your reverse proxy or tunnel at it;"
        say "docs/trying-the-alpha.md shows how."
    fi
}

main "$@"
