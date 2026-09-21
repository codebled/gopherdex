#!/usr/bin/env bash
# Checks a live Gopherdex deployment from the outside, the way users reach it.
#
#   deploy/smoke-test.sh https://gopherdex.example.com
#   deploy/smoke-test.sh https://gopherdex.example.com gopherdex.example.com/you/module@v0.1.0
#
# With a module, it also runs a real "go get" with default Go settings (no
# GOPROXY, GONOSUMDB or GOPRIVATE) in a throwaway module cache, which is the
# end-to-end test of zero-setup installs through the public mirror and
# checksum database.
set -euo pipefail

site=${1:?usage: smoke-test.sh SITE_URL [MODULE@VERSION]}
site=${site%/}
target=${2:-}
fail=0

pass() { printf '  ok    %s\n' "$1"; }
bad() { printf '  FAIL  %s\n' "$1"; fail=1; }

echo "Checking $site"

[[ $site == https://* ]] && pass "served over https" || bad "site must be https for zero-setup go get"
[[ $(curl -fsS "$site/healthz" || true) == ok ]] && pass "/healthz answers ok" || bad "/healthz"

headers=$(curl -fsSI "$site/" || true)
grep -qi '^strict-transport-security:' <<<"$headers" && pass "HSTS header" || bad "no Strict-Transport-Security header (is -base-url https?)"
grep -qi '^content-security-policy:' <<<"$headers" && pass "CSP header" || bad "no Content-Security-Policy header"
[[ $(curl -s -o /dev/null -w '%{http_code}' "http://${site#https://}/") =~ ^30[178]$ ]] && pass "http redirects to https" || bad "http:// doesn't redirect to https://"

curl -fsS "$site/robots.txt" | grep -q '^Sitemap:' && pass "robots.txt" || bad "robots.txt"
curl -fsS "$site/sitemap.xml" | grep -q '<urlset' && pass "sitemap.xml" || bad "sitemap.xml"
[[ $(curl -fsS "$site/api/oidc/audience" || true) == *audience* ]] && pass "trusted publishing is on" || echo "  note  trusted publishing is off"

if [[ -n $target ]]; then
	mod=${target%@*}
	version=${target#*@}
	[[ $mod == "$target" ]] && version=latest

	meta=$(curl -fsS "https://$mod?go-get=1" || true)
	grep -q "<meta name=\"go-import\" content=\"$mod mod $site/api/proxy\"" <<<"$meta" &&
		pass "go-import tag points the go command at $site/api/proxy" || bad "go-import tag for $mod"

	list=$(curl -fsS "$site/api/proxy/$mod/@v/list" || true)
	[[ -n $list ]] && pass "proxy lists versions: $(tr '\n' ' ' <<<"$list")" || bad "proxy has no versions of $mod"

	scratch=$(mktemp -d)
	trap 'chmod -R u+w "$scratch"; rm -rf "$scratch"' EXIT
	(
		cd "$scratch"
		export GOPATH="$scratch/gopath" GOMODCACHE="$scratch/gopath/pkg/mod" GOFLAGS=-modcacherw
		unset GOPROXY GONOSUMDB GONOSUMCHECK GOPRIVATE GONOPROXY GOSUMDB GOINSECURE
		go mod init example.com/smoke >/dev/null 2>&1
		go get "$mod@$version"
	) && pass "go get $mod@$version with default Go settings" || bad "go get $mod@$version (see output above)"
	if [[ -f $scratch/go.sum ]] && grep -q "^$mod " "$scratch/go.sum"; then
		pass "checksums recorded in go.sum (verified against sum.golang.org)"
		grep "^$mod " "$scratch/go.sum" | sed 's/^/        /'
	fi
fi

if ((fail)); then
	echo "Some checks failed."
	exit 1
fi
echo "All checks passed."
