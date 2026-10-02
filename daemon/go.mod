module github.com/Alexnex31/Norite/daemon

go 1.26.0

require (
	github.com/Alexnex31/Norite/backend v0.0.0
	github.com/Microsoft/go-winio v0.6.2
	github.com/coder/websocket v1.8.15
	github.com/fsnotify/fsnotify v1.10.1
	github.com/gofrs/flock v0.13.0
	github.com/rs/zerolog v1.35.1
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3
	github.com/stretchr/testify v1.11.1
	github.com/zalando/go-keyring v0.2.8
	golang.org/x/sys v0.47.0
	gopkg.in/natefinch/lumberjack.v2 v2.2.1
)

require (
	github.com/danieljoos/wincred v1.2.3 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/oapi-codegen/runtime v1.7.0 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/rogpeppe/go-internal v1.16.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// The backend module owns the gateway's wire format (backend/gatewayproto) and the contract's generated
// types (backend/apicontract), and from M19 the daemon speaks the one and decodes the other. A relative
// replace rather than a version, as cli/go.mod's are: these modules are developed and released together.
// Both packages sit outside backend/internal for exactly this edge, and both import nothing that decides
// anything — the rule that keeps a client module from reaching server logic through a shared format.
replace github.com/Alexnex31/Norite/backend => ../backend
