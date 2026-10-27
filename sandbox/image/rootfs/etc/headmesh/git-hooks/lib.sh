# shellcheck shell=bash
# Shared helpers for the global git hooks (core.hooksPath=/etc/headmesh/git-hooks).

hm_hook_fail() {
	printf 'headmesh %s: %s\n' "$HM_HOOK" "$*" >&2
	exit 1
}

# hm_runtime VAR: value from the environment, else from the container's runtime.env
# (login shells over SSH don't inherit the container environment).
hm_runtime() {
	local name=$1 value=${!1:-}
	if [[ -z $value && -r /run/hm-state/runtime.env ]]; then
		value=$(
			# shellcheck disable=SC1091
			. /run/hm-state/runtime.env >/dev/null 2>&1
			printf '%s' "${!name:-}"
		)
	fi
	printf '%s' "$value"
}

# hm_chain ARGS...: run the repository's own hook of the same name, if any, so
# global hooks don't silently disable a project's hooks. Uses the common git dir
# because `git rev-parse --git-path hooks` resolves to core.hooksPath (us).
hm_chain() {
	local common hook
	common=$(git rev-parse --git-common-dir) || return 0
	hook="$common/hooks/$HM_HOOK"
	if [[ -x $hook && $(readlink -f "$hook") != /etc/headmesh/git-hooks/* ]]; then
		exec "$hook" "$@"
	fi
	return 0
}
