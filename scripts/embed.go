// Package scripts embeds the guest-side shell and PowerShell scripts.
// They are kept as real .sh/.ps1 files so they stay editable as scripts;
// lefthook runs shellcheck/shfmt over the .sh ones.
package scripts

import _ "embed"

//go:embed guest/run-one-job.sh
var RunOneJob string

//go:embed guest/bake.sh
var Bake string

//go:embed docker/Dockerfile
var Dockerfile string

//go:embed docker/entrypoint.sh
var DockerEntrypoint string

//go:embed docker/entrypoint-slim.sh
var DockerEntrypointSlim string

//go:embed guest/windows/Unattend.xml
var WindowsUnattend string

//go:embed guest/windows/bake.ps1
var WindowsBake string

//go:embed guest/windows/run-one-job.ps1
var WindowsRunOneJob string
