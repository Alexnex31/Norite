module github.com/Alexnex31/Norite/cli

go 1.26.0

require (
	charm.land/bubbles/v2 v2.2.1
	charm.land/bubbletea/v2 v2.0.10
	charm.land/lipgloss/v2 v2.0.6
	github.com/Alexnex31/Norite/backend v0.0.0
	github.com/Alexnex31/Norite/daemon v0.0.0
	github.com/charmbracelet/x/ansi v0.11.8
	github.com/pelletier/go-toml/v2 v2.4.3
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3
	github.com/stretchr/testify v1.11.1
	github.com/urfave/cli/v3 v3.10.1
	golang.org/x/term v0.45.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/atotto/clipboard v0.1.4 // indirect
	github.com/charmbracelet/colorprofile v0.4.3 // indirect
	github.com/charmbracelet/ultraviolet v0.0.0-20260811164956-006e29f97886 // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/danieljoos/wincred v1.2.3 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/gofrs/flock v0.13.0 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/mattn/go-runewidth v0.0.27 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/oapi-codegen/runtime v1.7.0 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/stretchr/objx v0.5.3 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	github.com/zalando/go-keyring v0.2.8 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
)

// The daemon module owns what a stored credential is (ADR 0011), and the CLI writes one at
// `norite login`. A relative replace rather than a version: these modules are developed and released
// together in one repository and are never fetched independently.
replace github.com/Alexnex31/Norite/daemon => ../daemon

// The backend module owns the operator-token format (M10), and the CLI mints one at
// `norite instance bootstrap` — the one credential in this system minted by a client rather than a server,
// because what it proves is possession of a file the server cannot vouch for on the client's behalf. Only
// backend/operatortoken is reachable: everything else there is under internal/, which is the constraint
// that put the format in its own package rather than beside the code that verifies it.
replace github.com/Alexnex31/Norite/backend => ../backend
