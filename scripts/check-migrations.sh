#!/bin/sh
# check-migrations.sh fails if a pull request edits, renames or deletes an
# existing database migration. Released servers have already run those
# files, so changes must be a new, higher-numbered file instead.
# Usage: scripts/check-migrations.sh <base ref, e.g. origin/main>
set -eu

base=${1:?usage: check-migrations.sh <base-ref>}
changed=$(git diff --name-status "$base"...HEAD -- internal/database/migrations | grep -v '^A' || true)
if [ -n "$changed" ]; then
	echo "Existing migrations must not change. Add a new NNN_description.sql instead:"
	echo "$changed"
	exit 1
fi
