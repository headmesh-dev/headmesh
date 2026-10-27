# shellcheck shell=bash
# Helpers shared by the s6 service scripts (plan §6.7). Sourced, never executed.

HM_STATE_DIR=/run/hm-state
# shellcheck disable=SC2034 # used by the scripts that source this file
TS_SOCKET=/run/tailscale/tailscaled.sock

hm_log() {
	printf '%s %s: %s\n' "$(date -Is)" "${HM_SVC:-s6}" "$*" >&2
}

# hm_enabled NAME: true when NAME is in the comma-separated HEADMESH_ENTRYPOINTS.
hm_enabled() {
	case ",${HEADMESH_ENTRYPOINTS:-}," in
	*",$1,"*) return 0 ;;
	esac
	return 1
}

# hm_state STATE [REASON]: record this service's state for `sandbox_status`.
hm_state() {
	[[ -d $HM_STATE_DIR ]] || mkdir -p "$HM_STATE_DIR"
	local file="$HM_STATE_DIR/${HM_SVC}.json"
	local json
	json=$(jq -cn --arg service "$HM_SVC" --arg state "$1" --arg reason "${2:-}" \
		--arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
		'{service: $service, state: $state, reason: $reason, ts: $ts}')
	if [[ -w $HM_STATE_DIR ]]; then
		printf '%s\n' "$json" >"$file.tmp" && chmod 0644 "$file.tmp" && mv -f "$file.tmp" "$file"
	else
		# Unprivileged services write in place to a file secrets-init created for them.
		printf '%s\n' "$json" >"$file"
	fi
}

# hm_idle REASON: the front door is not configured; stay up (so s6-rc is happy)
# but do nothing, and say why in status.
hm_idle() {
	hm_state disabled "$1"
	hm_log "idle: $1"
	exec s6-pause
}

# hm_put_file SRC DST MODE OWNER: copy as root, set the mode, then hand the file
# over. Order matters: without CAP_FOWNER, root can't chmod a file it no longer
# owns, and Ubuntu 26.04's uutils `install -o` chowns before it chmods.
hm_put_file() {
	rm -f "$2"
	cp "$1" "$2"
	chmod "$3" "$2"
	chown "$4:$4" "$2"
}

# hm_owned_dir DIR MODE OWNER: create DIR if missing, with mode and owner.
hm_owned_dir() {
	mkdir -p "$1"
	chmod "$2" "$1"
	chown "$3:$3" "$1"
}

# hm_agent_env: the environment agent-owned processes expect. It doesn't change
# directory: s6 tools such as s6-notifyoncheck read files from the service dir.
hm_agent_env() {
	export HOME=/home/agent USER=agent LOGNAME=agent SHELL=/bin/bash
}
