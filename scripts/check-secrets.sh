#!/usr/bin/env bash
# Fail when a credential-shaped string shows up in the working tree or anywhere in history.
#
# Fixtures and tests are allowed to look realistic, but they have to be *visibly* synthetic:
# every allowed value must start with one of the prefixes below, so a captured key or
# request id is always caught, including one that was committed and then removed (the
# history scan is the point of this script).
#
#   scripts/check-secrets.sh            # scan, exit 1 on any hit
#   scripts/check-secrets.sh --verbose  # also list the allowed synthetic values
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

verbose=0
[ "${1:-}" = "--verbose" ] && verbose=1

# Credential and captured-identifier shapes. Keep this list in sync with the fixtures'
# synthetic prefixes below.
patterns='sk-[A-Za-z0-9_-]{12,}'
patterns+='|sk_[A-Za-z0-9]{16,}'
patterns+='|AIza[0-9A-Za-z_-]{30,}'
patterns+='|(ghp|gho|ghs|ghu|github_pat)_[A-Za-z0-9_]{20,}'
patterns+='|xox[baprs]-[A-Za-z0-9-]{10,}'
patterns+='|eyJ[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}'
patterns+='|BEGIN [A-Z ]*PRIVATE KEY'
patterns+='|gen_[A-Za-z0-9]{12,}'
patterns+='|fp_[a-z0-9]{8,}'
patterns+='|codex-[A-Za-z0-9-]{20,}'
patterns+='|usr-[A-Za-z0-9]{8,}'

# Synthetic values this repository deliberately uses in fixtures and tests.
allowed='sk-TESTKEY|sk-FIXTURE|sk-EXAMPLE|gen_FIXTURE|fp_fixture|codex-fixture|usr-fixture'

mask() {
	local value="$1"
	if [ "${#value}" -le 8 ]; then
		printf '***'
		return
	fi
	printf '%s…%s' "${value:0:4}" "${value: -2}"
}

hits=0

scan() { # <where> <text>
	local where="$1" text="$2" match
	while IFS= read -r match; do
		[ -n "$match" ] || continue
		if printf '%s' "$match" | grep -qE "^(${allowed})"; then
			[ "$verbose" -eq 1 ] && printf '  allowed  %-52s %s\n' "$where" "$(mask "$match")"
			continue
		fi
		printf '  HIT      %-52s %s\n' "$where" "$(mask "$match")"
		hits=$((hits + 1))
	done < <(printf '%s\n' "$text" | grep -aoE "$patterns" | sort -u | head -5)
}

echo "checking the working tree"
while IFS= read -r file; do
	[ -f "$file" ] || continue
	case "$file" in
	*.so | *.dylib | *.dll) continue ;;
	esac
	scan "$file" "$(grep -I -a . "$file" 2>/dev/null || true)"
done < <(git ls-files)

echo "checking every blob in the git history"
while IFS= read -r object; do
	[ "$(git cat-file -t "$object" 2>/dev/null)" = "blob" ] || continue
	paths="$(git rev-list --all --objects | awk -v sha="$object" '$1 == sha {print $2}' | sort -u | head -2 | paste -sd, -)"
	scan "${object:0:10} ${paths}" "$(git cat-file blob "$object" 2>/dev/null | grep -I -a . || true)"
done < <(git rev-list --objects --all | awk '{print $1}' | sort -u)

if [ "$hits" -gt 0 ]; then
	echo
	echo "check-secrets: $hits credential-shaped value(s) outside the synthetic allowlist" >&2
	exit 1
fi

echo "check-secrets: clean"
