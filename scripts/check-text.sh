#!/bin/sh
# check-text.sh fails if a tracked text file has trailing whitespace or no
# final newline. Binary files (images, icons, fonts) are skipped. Run it
# with `make text`.
set -eu

status=0
# Trailing spaces or tabs at the end of a line.
if git grep -nIE '[[:blank:]]+$' -- . ':!*.svg' >/tmp/gdx-trailing.$$ 2>/dev/null; then
	echo "Trailing whitespace (remove it, or let .editorconfig do it):"
	cat /tmp/gdx-trailing.$$
	status=1
fi
rm -f /tmp/gdx-trailing.$$

# A final newline in every non-empty text file.
for f in $(git ls-files); do
	[ -f "$f" ] && [ -s "$f" ] || continue
	grep -Iq . "$f" 2>/dev/null || continue # binary
	if [ "$(tail -c 1 "$f" | od -An -c | tr -d ' ')" != '\n' ]; then
		echo "No newline at the end of $f"
		status=1
	fi
done
exit $status
