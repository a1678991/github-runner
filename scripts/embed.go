// Package scripts embeds the guest-side shell scripts. They are kept as
// real .sh files so shellcheck/shfmt/lefthook cover them.
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
