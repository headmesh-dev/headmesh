# shellcheck shell=sh
# Sandbox login-shell environment (plan §6.2, §6.11). Sourced by /etc/profile, so it
# applies to Tailscale SSH logins (Codex remote connections), `bash -l` and
# sandboxd's `bash -lc` exec. Login shells don't inherit the container
# environment, so mode and hostname come from /run/hm-state/runtime.env, which
# secrets-init writes at container start. Keep this POSIX sh.

if [ -r /run/hm-state/runtime.env ]; then
	# shellcheck disable=SC1091
	. /run/hm-state/runtime.env
fi

export MISE_DATA_DIR=/cache/mise
export MISE_CACHE_DIR=/cache/mise/cache
export UV_CACHE_DIR=/cache/uv
export UV_TOOL_DIR=/cache/uv/tools
export UV_PYTHON_INSTALL_DIR=/cache/uv/python
export npm_config_cache=/cache/npm
export NPM_CONFIG_PREFIX=/cache/npm-global
export npm_config_store_dir=/cache/pnpm/store
export GOPATH=/cache/go
export GOMODCACHE=/cache/go/pkg/mod
export GOCACHE=/cache/go/build
export CARGO_HOME=/cache/cargo
export DISABLE_AUTOUPDATER=1
export HEADMESH_SANDBOX_CONFIG=/etc/headmesh/config/sandbox.toml

# Order: project tools from mise.toml (shims), user-installed tools, then the
# toolchains baked into the image, then the system.
hm_path_prepend() {
	case ":$PATH:" in
	*":$1:"*) ;;
	*) PATH="$1:$PATH" ;;
	esac
}
hm_path_prepend /command
hm_path_prepend /usr/local/share/mise/bin
hm_path_prepend /cache/cargo/bin
hm_path_prepend /cache/go/bin
hm_path_prepend /cache/npm-global/bin
hm_path_prepend /cache/mise/shims
[ -n "${HOME:-}" ] && hm_path_prepend "$HOME/.local/bin"
unset -f hm_path_prepend
export PATH

if [ -z "${GH_TOKEN:-}" ] && [ -r /run/hm-secrets/github_token ]; then
	GH_TOKEN=$(cat /run/hm-secrets/github_token)
	export GH_TOKEN
fi

# Commit attribution (commit-msg hook) for interactive SSH logins.
if [ -n "${SSH_CONNECTION:-}" ] && [ -z "${HEADMESH_ENTRYPOINT:-}" ]; then
	export HEADMESH_ENTRYPOINT=ssh
	export HEADMESH_SESSION="ssh-$$"
fi

# Interactive bash: mode in the prompt ([YOLO] in red) and the banner. ~/.bashrc runs
# after this file and usually sets its own PS1, so the marker is (re)added from
# PROMPT_COMMAND instead of being set once.
if [ -n "${BASH_VERSION:-}" ] && [ -n "${PS1:-}" ]; then
	if [ "${HEADMESH_MODE:-regular}" = yolo ]; then
		HEADMESH_PS1_MARK='\[\e[1;31m\][YOLO]\[\e[0m\] '
	else
		HEADMESH_PS1_MARK='[sandbox] '
	fi
	headmesh_ps1() {
		case $PS1 in
		"$HEADMESH_PS1_MARK"*) ;;
		*) PS1="$HEADMESH_PS1_MARK$PS1" ;;
		esac
	}
	PROMPT_COMMAND="headmesh_ps1${PROMPT_COMMAND:+;$PROMPT_COMMAND}"
	if [ -r /run/headmesh/banner ] && [ -z "${HEADMESH_BANNER_SHOWN:-}" ]; then
		cat /run/headmesh/banner
		export HEADMESH_BANNER_SHOWN=1
	fi
fi
