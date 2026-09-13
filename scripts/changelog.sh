#!/usr/bin/env bash
# Prints a grouped Markdown changelog for a commit range.
#
#   changelog.sh [<previous-ref>] [<current-ref>]
#
# With no <previous-ref> the whole history up to <current-ref> is used.
# Merge commits and the release workflow's own "bump version" commits are
# left out. Prints nothing when the range holds no reportable commits.
set -euo pipefail

from="${1:-}"
to="${2:-HEAD}"

range="$to"
if [ -n "$from" ]; then
	range="$from..$to"
fi

breaking=()
features=()
fixes=()
docs=()
maint=()
other=()

while IFS= read -r -d $'\x1e' record; do
	[ -n "$record" ] || continue
	record="${record#$'\n'}"

	sha="${record%%$'\x1f'*}"
	rest="${record#*$'\x1f'}"
	subject="${rest%%$'\x1f'*}"
	body="${rest#*$'\x1f'}"

	[ -n "$sha" ] || continue
	case "$subject" in
	"chore: bump version to "* | "chore("*"): bump version to "*) continue ;;
	esac

	type=""
	scope=""
	bang=""
	desc="$subject"
	if [[ "$subject" =~ ^([a-zA-Z]+)(\(([^\)]*)\))?(!)?:[[:space:]]*(.*)$ ]]; then
		type="${BASH_REMATCH[1]}"
		scope="${BASH_REMATCH[3]}"
		bang="${BASH_REMATCH[4]}"
		desc="${BASH_REMATCH[5]}"
	fi

	if [ -n "$scope" ]; then
		entry="- **${scope}**: ${desc} (${sha})"
	else
		entry="- ${desc} (${sha})"
	fi

	if [ -n "$bang" ] || [[ "$body" == *"BREAKING CHANGE"* ]]; then
		breaking+=("$entry")
		continue
	fi

	case "$type" in
	feat) features+=("$entry") ;;
	fix) fixes+=("$entry") ;;
	docs) docs+=("$entry") ;;
	chore | build | ci | perf | refactor | style | test) maint+=("$entry") ;;
	*) other+=("$entry") ;;
	esac
done < <(git log --no-merges --reverse --pretty=format:"%h%x1f%s%x1f%b%x1e" "$range")

total=$((${#breaking[@]} + ${#features[@]} + ${#fixes[@]} + ${#docs[@]} + ${#maint[@]} + ${#other[@]}))
if [ "$total" -eq 0 ]; then
	exit 0
fi

section() {
	local title=$1
	shift
	[ "$#" -gt 0 ] || return 0
	printf '### %s\n' "$title"
	printf '%s\n' "$@"
	printf '\n'
}

printf '## Changes\n\n'
section "⚠️ Breaking changes" ${breaking[@]+"${breaking[@]}"}
section "Features" ${features[@]+"${features[@]}"}
section "Fixes" ${fixes[@]+"${fixes[@]}"}
section "Documentation" ${docs[@]+"${docs[@]}"}
section "Maintenance" ${maint[@]+"${maint[@]}"}
section "Other" ${other[@]+"${other[@]}"}
