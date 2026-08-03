# shellcrumbs — zsh integration
# eval "$(shcr init zsh)"

[[ -n "${__SHCR_LOADED:-}" ]] && return 0
__SHCR_LOADED=1
__SHCR_BIN='@SHCR_BIN@'

# The full module, not just b:strftime — EPOCHREALTIME is a parameter, and a
# builtin-only load leaves it unset, which would silently fall back to a clock
# measured from shell startup.
zmodload zsh/datetime 2>/dev/null
autoload -Uz add-zsh-hook

if [[ -z "${SHCR_SESSION_ID:-}" ]]; then
  if [[ -n "${EPOCHREALTIME:-}" ]]; then
    export SHCR_SESSION_ID="${HOST:-host}-$$-${${EPOCHREALTIME/,/.}//./}"
  else
    export SHCR_SESSION_ID="${HOST:-host}-$$-${RANDOM}${RANDOM}"
  fi
fi

typeset -g __shcr_seq=0
typeset -g __shcr_cmd_id=''
typeset -g __shcr_bg=''
typeset -g __shcr_ms=0

# Assigns to __shcr_ms instead of printing: reading it back with $(...) would
# fork a subshell on every prompt.
__shcr_now_ms() {
  local t="${EPOCHREALTIME:-}"
  if [[ -n "$t" ]]; then
    t="${t/,/.}"
    local s="${t%%.*}" frac="${t#*.}000"
    __shcr_ms="${s}${frac[1,3]}"
  else
    # zsh/datetime did not load, so EPOCHREALTIME and EPOCHSECONDS are both
    # missing. SECONDS is measured from shell startup — the very failure the
    # note above this file's zmodload warns about — so it would date every
    # command to just after 1970. A fork is the lesser cost.
    __shcr_ms="$(date +%s)000"
  fi
}

__shcr_send() {
  ( "$__SHCR_BIN" "$@" >/dev/null 2>&1 & ) 2>/dev/null
}

# Command text travels in the environment: /proc/<pid>/cmdline is world-readable
# and /proc/<pid>/environ is not, so an argument would publish every recorded
# command to every other account on the machine.
__shcr_send_command() {
  ( SHCR_COMMAND="$1" "$__SHCR_BIN" "${@:2}" >/dev/null 2>&1 & ) 2>/dev/null
}

__shcr_preexec() {
  # extended_glob is what makes the [[:space:]]## quantifier below mean "one or
  # more"; without it the pattern is inert and trailing whitespace defeats the
  # background check. localoptions puts the user's settings back on return.
  setopt localoptions extended_glob

  # $1 is the line exactly as typed, newlines and all.
  local cmd="$1"
  [[ -z "$cmd" ]] && return
  if [[ -o hist_ignore_space && "$cmd" == [[:space:]]* ]]; then
    return
  fi

  local trimmed="${cmd%%[[:space:]]##}"
  __shcr_bg=''
  case "$trimmed" in
    *'&&') ;;
    *'&') __shcr_bg='--background' ;;
  esac

  __shcr_seq=$(( __shcr_seq + 1 ))
  __shcr_cmd_id="${SHCR_SESSION_ID}.${__shcr_seq}"

  __shcr_now_ms
  __shcr_send_command "$cmd" event start \
    --id "$__shcr_cmd_id" \
    --session "$SHCR_SESSION_ID" \
    --cwd "$PWD" \
    --shell zsh \
    --pgid "$$" \
    --time "$__shcr_ms" \
    ${__shcr_bg}
}

__shcr_precmd() {
  # Must be the first statement, and this hook must be first in the chain —
  # any other precmd running ahead of us would have replaced $?.
  # Note: `status` is read-only in zsh, so the obvious name is not available.
  local ret=$?
  [[ -z "$__shcr_cmd_id" ]] && return
  # A backgrounded command is still running even though the prompt is back.
  if [[ -z "$__shcr_bg" ]]; then
    __shcr_now_ms
    __shcr_send event end \
      --id "$__shcr_cmd_id" \
      --exit "$ret" \
      --time "$__shcr_ms"
  fi
  __shcr_cmd_id=''
  __shcr_bg=''
}

add-zsh-hook preexec __shcr_preexec
add-zsh-hook precmd __shcr_precmd

# add-zsh-hook appends, which would leave anything already installed (p10k,
# starship, a theme's precmd) running ahead of us and clobbering $?. Move both
# hooks to the front; tools loaded after us append behind, which is fine.
precmd_functions=(__shcr_precmd ${precmd_functions:#__shcr_precmd})
preexec_functions=(__shcr_preexec ${preexec_functions:#__shcr_preexec})

# ---------------------------------------------------------------- Ctrl+R

# A zle widget, so nothing here reaches preexec and the picker is never
# recorded as a command.
__shcr_pick() {
  local out
  out="$("$__SHCR_BIN" tui --query "$BUFFER" < /dev/tty)"
  if [[ -n "$out" ]]; then
    BUFFER="$out"
    CURSOR=${#BUFFER}
  fi
  zle reset-prompt
}

if [[ -z "${SHCR_NO_BIND:-}" ]]; then
  zle -N __shcr_pick
  bindkey '^R' __shcr_pick
fi

# ---------------------------------------------------------------- sync hints
#
# Sitting down at a terminal is when other machines' history is most worth
# having; closing one is the last chance to push what just happened.

__shcr_nudge() {
  ( "$__SHCR_BIN" nudge "$1" >/dev/null 2>&1 & ) 2>/dev/null
}

__shcr_on_exit() { __shcr_nudge session-end }
add-zsh-hook zshexit __shcr_on_exit

__shcr_nudge session-start
