module github.com/codesweep-ai/vcr

go 1.27.0

require (
	github.com/spf13/cobra v1.10.2
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/codesweep-ai/ledger v0.0.0-20260826154712-f3d4cf8989eb // indirect
	github.com/codesweep-ai/lint v0.0.0-20260826152054-3acef36b8e16 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
)

tool (
	github.com/codesweep-ai/ledger/cmd/cs-ledger
	github.com/codesweep-ai/lint/cmd/cs-lint
)
