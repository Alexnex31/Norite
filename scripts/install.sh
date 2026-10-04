#!/bin/sh
#
# Install the Norite client for the current user, on macOS or Linux: `norite` (the command line and the
# terminal client) and `norite-daemon` (the background program it talks to). It never asks for root.
#
#   curl -fsSL https://raw.githubusercontent.com/Alexnex31/Norite/main/scripts/install.sh | sh
#   curl -fsSL …/scripts/install.sh | sh -s -- --instance https://chat.example.com
#   ./scripts/install.sh                      # in a checkout: builds from that source instead
#
# What it does, in order: gets the two programs (a release archive, checked, or a build of the checkout it
# sits in), puts them in ~/.local/bin, registers the daemon with the service manager and starts it, then
# offers to create an account or sign in. `--help` lists the switches for doing less.
#
# POSIX sh, not bash: it is piped into whatever `sh` a machine has, which is dash on Debian and bash 3.2 on
# macOS. The whole of it is inside main(), called on the last line, so a download cut short runs nothing
# rather than half of something.

set -eu

REPO="Alexnex31/Norite"

say()  { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

have() { command -v "$1" >/dev/null 2>&1; }

usage() {
    cat <<'EOF'
Install the Norite client: norite and norite-daemon.

  install.sh [options]

Where the programs come from:
  --version TAG         install this release, e.g. v0.1.0-alpha (default: the newest)
  --from-source         build the checkout this script is in (the default when it is in one; needs Go)
  --release             download a release even when run from a checkout

Where they go, and what else happens:
  --bin-dir DIR         install into DIR (default: ~/.local/bin)
  --instance URL        the instance to register on or sign in to (default: ask)
  --invite-code CODE    the instance invite code, for an instance that needs one to register
  --no-daemon           do not register or start the daemon
  --no-account          do not offer to register or sign in
  --require-signature   fail unless cosign is installed and the release's signature verifies
  --with-server         also install norite-server (install-server.sh uses this)
  --no-summary          do not print the closing notes (install-server.sh prints its own)
  --uninstall           sign out, remove the daemon's registration and the programs
  -h, --help            this text
EOF
}

# ---------- fetching ----------

# fetch URL DEST. HTTPS is enforced for an https URL, so a redirect cannot downgrade it; a plain-http URL
# only ever comes from NORITE_RELEASE_BASE_URL, which exists for testing against a local directory.
fetch() {
    if have curl; then
        case "$1" in
            https://*) curl -fsSL --proto '=https' --tlsv1.2 -o "$2" "$1" ;;
            *)         curl -fsSL -o "$2" "$1" ;;
        esac
    elif have wget; then
        wget -q -O "$2" "$1"
    else
        die "curl or wget is needed to download Norite"
    fi
}

# newest_tag prints the newest release's tag. The list endpoint rather than /releases/latest, because
# "latest" skips prereleases and the alpha is one.
newest_tag() {
    fetch "https://api.github.com/repos/$REPO/releases?per_page=1" "$tmp/releases.json" ||
        die "could not ask GitHub for the newest release; pass --version TAG, or check the network"
    sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$tmp/releases.json" | head -n 1
}

sha256_of() {
    if have sha256sum; then sha256sum "$1" | awk '{print $1}'
    elif have shasum; then shasum -a 256 "$1" | awk '{print $1}'
    else die "sha256sum or shasum is needed to check the download"
    fi
}

# check_archive NAME: the file in $tmp must be listed in checksums.txt with the hash it actually has.
check_archive() {
    want=$(awk -v f="$1" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")
    [ -n "$want" ] || die "$1 is not listed in this release's checksums.txt"
    got=$(sha256_of "$tmp/$1")
    [ "$want" = "$got" ] || die "$1 does not match its checksum (wanted $want, got $got); not installing it"
}

# download_release fills $tmp/out with the programs from a release archive, after checking them.
download_release() {
    [ -n "$tag" ] || tag=$(newest_tag)
    [ -n "$tag" ] || die "no release is published yet. In a checkout, run this script with --from-source"
    # The tag becomes part of a URL and a file name, so it is held to what a tag is made of.
    case "$tag" in
        *[!A-Za-z0-9._+-]*) die "'$tag' is not a release tag" ;;
    esac
    version=${tag#v}
    base=${NORITE_RELEASE_BASE_URL:-https://github.com/$REPO/releases/download/$tag}

    step "Downloading Norite $tag for $os/$arch"
    fetch "$base/checksums.txt" "$tmp/checksums.txt" || die "could not download checksums.txt for $tag from $base"

    # The signature is over checksums.txt, so it is checked before anything checksums.txt vouches for.
    if have cosign; then
        fetch "$base/checksums.txt.sigstore.json" "$tmp/checksums.txt.sigstore.json" ||
            die "this release has no signature bundle beside its checksums; not installing it"
        cosign verify-blob "$tmp/checksums.txt" \
            --bundle "$tmp/checksums.txt.sigstore.json" \
            --certificate-identity "https://github.com/$REPO/.github/workflows/release.yml@refs/tags/$tag" \
            --certificate-oidc-issuer https://token.actions.githubusercontent.com >/dev/null 2>"$tmp/cosign.err" ||
            { cat "$tmp/cosign.err" >&2; die "the release's signature did not verify; not installing it"; }
        say "signature: verified, signed by this repository's release workflow at $tag"
    elif [ "$require_signature" = 1 ]; then
        die "--require-signature needs cosign: https://docs.sigstore.dev/cosign/system_config/installation/"
    else
        say "signature: not checked, because cosign is not installed. The checksum below proves the"
        say "           download is intact, not who built it; install cosign and pass --require-signature for that."
    fi

    archives="norite_${version}_${os}_${arch}.tar.gz"
    [ "$with_server" = 1 ] && archives="$archives norite-server_${version}_${os}_${arch}.tar.gz"
    mkdir -p "$tmp/out"
    for a in $archives; do
        fetch "$base/$a" "$tmp/$a" || die "could not download $a; is there a build of $tag for $os/$arch?"
        check_archive "$a"
        say "checksum:  $a matches"
        tar -xzf "$tmp/$a" -C "$tmp/out"
    done
}

# build_source fills $tmp/out by building the checkout, the way a release builds: no cgo, no workspace.
build_source() {
    [ -n "$checkout" ] || die "--from-source needs this script to be run from a checkout of the repository"
    have go || die "building from source needs Go 1.26 or newer: https://go.dev/dl/"
    step "Building Norite from $checkout"
    mkdir -p "$tmp/out"
    (cd "$checkout/cli" && CGO_ENABLED=0 GOWORK=off go build -trimpath -o "$tmp/out/norite" ./cmd/app)
    (cd "$checkout/daemon" && CGO_ENABLED=0 GOWORK=off go build -trimpath -o "$tmp/out/norite-daemon" ./cmd/daemond)
    if [ "$with_server" = 1 ]; then
        (cd "$checkout/backend" && CGO_ENABLED=0 GOWORK=off go build -trimpath -o "$tmp/out/norite-server" ./cmd/server)
    fi
}

# ---------- installing ----------

# place NAME: copy beside the destination and rename over it. A rename replaces a program that is running
# (the daemon, on an upgrade); a copy onto it fails with "text file busy".
place() {
    [ -f "$tmp/out/$1" ] || die "$1 is missing from what was fetched"
    cp "$tmp/out/$1" "$bin_dir/.$1.new.$$"
    chmod 0755 "$bin_dir/.$1.new.$$"
    mv -f "$bin_dir/.$1.new.$$" "$bin_dir/$1"
}

on_path() {
    case ":$PATH:" in
        *":$bin_dir:"*) return 0 ;;
        *) return 1 ;;
    esac
}

setup_daemon() {
    step "Setting up the daemon"
    # install replaces an earlier registration; restart is stop-then-start and each half is a no-op when
    # there is nothing to stop or it is already running, so this is right for a first install and an upgrade.
    if "$norite" daemon install >"$tmp/daemon.log" 2>&1 && "$norite" daemon restart >>"$tmp/daemon.log" 2>&1; then
        say "The daemon is running, and starts by itself when you log in."
        return 0
    fi
    sed 's/^/  /' "$tmp/daemon.log" >&2
    warn "the daemon could not be registered with this machine's service manager."
    say "Run it by hand when you want to use Norite, and leave it running:"
    say "  $bin_dir/norite-daemon &"
}

# ---------- the account ----------

# A script piped into sh has the pipe as its stdin, so its questions, and norite's own prompts, are read
# from the terminal itself.
have_tty() { (: </dev/tty) 2>/dev/null; }

ask() { # ask PROMPT → $answer
    printf '%s' "$1" >/dev/tty
    IFS= read -r answer </dev/tty || answer=""
}

setup_account() {
    if ! have_tty; then
        say ""
        say "No terminal to ask on, so no account was set up. When you are ready:"
        say "  norite register --instance ${instance:-INSTANCE-URL}    # or: norite login --instance …"
        return 0
    fi
    step "Your account"
    if [ -z "$instance" ]; then
        ask "Instance address, e.g. https://chat.example.com (Enter to skip): "
        instance=$answer
    fi
    if [ -z "$instance" ]; then
        say "Skipped. Later: norite register --instance URL, or norite login --instance URL"
        return 0
    fi
    ask "Do you already have an account on $instance? [y/N] "
    existing=$answer
    # Asked here and handed to both commands, so it is typed once rather than once each.
    ask "Email: "
    email=$answer
    [ -n "$email" ] || { say "No email given, so no account was set up."; return 0; }
    case "$existing" in
        y | Y | yes | Yes | YES) ;;
        *)
            if [ -z "$invite_code" ]; then
                ask "Instance invite code, if this instance needs one to register (Enter for none): "
                invite_code=$answer
            fi
            set -- register --instance "$instance" --email "$email"
            [ -n "$invite_code" ] && set -- "$@" --invite-code "$invite_code"
            if ! "$norite" "$@" </dev/tty; then
                warn "registering did not finish. Try again with: norite register --instance $instance"
                return 0
            fi
            say ""
            ;;
    esac
    "$norite" login --instance "$instance" --email "$email" </dev/tty ||
        warn "signing in did not finish. Try again with: norite login --instance $instance"
}

# ---------- uninstalling ----------

uninstall() {
    norite="$bin_dir/norite"
    [ -x "$norite" ] || die "there is no norite in $bin_dir; pass --bin-dir if it was installed elsewhere"
    step "Removing Norite from $bin_dir"
    "$norite" logout >/dev/null 2>&1 || true
    "$norite" daemon uninstall >/dev/null 2>&1 || true
    rm -f "$bin_dir/norite" "$bin_dir/norite-daemon"
    say "Removed. The daemon's state directory is left in place; delete it for a clean slate:"
    case "$os" in
        darwin) say "  ~/Library/Application Support/Norite" ;;
        *)      say "  ${XDG_STATE_HOME:-$HOME/.local/state}/norite" ;;
    esac
}

# ---------- main ----------

main() {
    tag="" mode="" bin_dir="$HOME/.local/bin" instance="" invite_code=""
    daemon=1 account=1 require_signature=0 with_server=0 do_uninstall=0 summary=1

    while [ $# -gt 0 ]; do
        case "$1" in
            --version)           [ $# -ge 2 ] || die "--version needs a tag"; tag=$2; mode=release; shift ;;
            --from-source)       mode=source ;;
            --release)           mode=release ;;
            --bin-dir)           [ $# -ge 2 ] || die "--bin-dir needs a directory"; bin_dir=$2; shift ;;
            --instance)          [ $# -ge 2 ] || die "--instance needs a URL"; instance=$2; shift ;;
            --invite-code)       [ $# -ge 2 ] || die "--invite-code needs a code"; invite_code=$2; shift ;;
            --no-daemon)         daemon=0 ;;
            --no-account)        account=0 ;;
            --require-signature) require_signature=1 ;;
            --with-server)       with_server=1 ;;
            --no-summary)        summary=0 ;;
            --uninstall)         do_uninstall=1 ;;
            -h | --help)         usage; exit 0 ;;
            *)                   usage >&2; die "unknown option: $1" ;;
        esac
        shift
    done

    case "$(uname -s)" in
        Linux)  os=linux ;;
        Darwin) os=darwin ;;
        *)      die "this script is for macOS and Linux. On Windows, use scripts/install.ps1" ;;
    esac
    case "$(uname -m)" in
        x86_64 | amd64)  arch=amd64 ;;
        arm64 | aarch64) arch=arm64 ;;
        *)               die "Norite is built for x86-64 and ARM64, not $(uname -m)" ;;
    esac

    if [ "$do_uninstall" = 1 ]; then
        uninstall
        exit 0
    fi

    # A script run from a checkout builds that checkout unless told otherwise: whoever has the source in
    # front of them wants what it says, not the last release. Piped into sh there is no file, so no checkout.
    checkout=""
    case "$0" in
        */*)
            dir=$(cd "$(dirname "$0")" 2>/dev/null && pwd) || dir=""
            if [ -n "$dir" ] && [ -f "$dir/../go.work" ] && [ -d "$dir/../cli" ] && [ -d "$dir/../daemon" ]; then
                checkout=$(cd "$dir/.." && pwd)
            fi
            ;;
    esac
    if [ -z "$mode" ]; then
        if [ -n "$checkout" ]; then mode=source; else mode=release; fi
    fi

    tmp=$(mktemp -d 2>/dev/null || mktemp -d -t norite-install)
    trap 'rm -rf "$tmp"' EXIT INT TERM

    case "$mode" in
        source)  build_source ;;
        release) download_release ;;
    esac

    step "Installing into $bin_dir"
    mkdir -p "$bin_dir"
    place norite
    place norite-daemon
    [ "$with_server" = 1 ] && place norite-server
    norite="$bin_dir/norite"
    "$norite" --help >/dev/null 2>&1 || die "$norite was installed but does not run on this machine"
    say "norite, norite-daemon$([ "$with_server" = 1 ] && printf ', norite-server')"

    [ "$daemon" = 1 ] && setup_daemon
    [ "$account" = 1 ] && setup_account
    [ "$summary" = 1 ] || return 0

    step "Done"
    if on_path; then
        say "Run 'norite' to open the client, and 'norite --help' for everything else."
    else
        say "$bin_dir is not on your PATH yet. Add this line to your shell's startup file"
        say "(~/.zshrc on macOS, ~/.bashrc on most Linux), then open a new terminal:"
        say ""
        say "  export PATH=\"$bin_dir:\$PATH\""
        say ""
        say "Until then, run the client as: $norite"
    fi
}

main "$@"
