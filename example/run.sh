#!/bin/sh
# Builds tsbridge and runs it (pointed at config.yaml, in this same
# directory) alongside Caddy. Set TS_AUTHKEY first for unattended
# registration -- see README.md's "Registering the bridge node" section
# otherwise. Ctrl+C stops both.
set -eu
cd "$(dirname "$0")"
go build -o ../tsbridge ..
../tsbridge -config config.yaml &
trap 'kill $!' EXIT
caddy run
