module github.com/Alexnex31/Norite/cli

go 1.25.0

require (
	github.com/Alexnex31/Norite/daemon v0.0.0
	github.com/pelletier/go-toml/v2 v2.4.3
	github.com/stretchr/testify v1.11.1
	github.com/urfave/cli/v3 v3.10.1
	golang.org/x/term v0.45.0
)

require (
	github.com/danieljoos/wincred v1.2.3 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/gofrs/flock v0.13.0 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/zalando/go-keyring v0.2.8 // indirect
	golang.org/x/sys v0.47.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// The daemon module owns what a stored credential is (ADR 0011), and the CLI writes one at
// `norite login`. A relative replace rather than a version: these modules are developed and released
// together in one repository and are never fetched independently.
replace github.com/Alexnex31/Norite/daemon => ../daemon
