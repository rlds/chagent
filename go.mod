module github.com/rlds/chagent

go 1.27.3

require (
	github.com/adhocore/gronx v1.20.0
	github.com/caarlos0/env/v11 v11.4.1
	github.com/creack/pty v1.1.24
	github.com/gomarkdown/markdown v0.0.0-20260411013819-759bbc3e3207
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/h2non/filetype v1.1.3
	github.com/kagisearch/kagi-openapi-golang v0.0.0-20260526215348-96575e864d62
	github.com/modelcontextprotocol/go-sdk v1.6.1
	github.com/pmezard/go-difflib v1.0.0
	github.com/rs/zerolog v1.35.1
	github.com/spf13/cobra v1.10.2
	github.com/stretchr/testify v1.11.1
	golang.org/x/crypto v0.51.0
	golang.org/x/net v0.55.0
	golang.org/x/sys v0.46.0
	golang.org/x/term v0.44.0
	golang.org/x/time v0.16.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/rogpeppe/go-internal v1.13.1 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
)

// Local fork: enable native MCP protocol 2026-06-30 support (see third_party/mcp-go-sdk/PATCH.md).
replace github.com/modelcontextprotocol/go-sdk => ./third_party/mcp-go-sdk
