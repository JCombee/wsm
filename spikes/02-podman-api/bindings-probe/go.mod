module spike/bindingsprobe

go 1.24

// Matches the Podman in Debian 13. run.sh adds go.sum with go mod tidy.
require github.com/containers/podman/v5 v5.4.2
