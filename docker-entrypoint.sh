#!/bin/sh
set -eu

mkdir -p /data
chown -R omni:omni /data

if ! su-exec omni test -w /data; then
	echo "data directory /data is not writable by uid 10001" >&2
	exit 1
fi

exec su-exec omni /usr/local/bin/omni-api "$@"
