# shellcrumbs — bash integration
# eval "$(shcr init bash)"

[ -n "${__SHCR_LOADED:-}" ] && return 0
__SHCR_LOADED=1
__SHCR_BIN='@SHCR_BIN@'

# One session id per shell, exported so subshells and nested shells stay in the
# same session.
if [ -z "${SHCR_SESSION_ID:-}" ]; then
  if [ -n "${EPOCHREALTIME:-}" ]; then
    __shcr_stamp="${EPOCHREALTIME/,/.}"
    SHCR_SESSION_ID="${HOSTNAME:-host}-$$-${__shcr_stamp//./}"
    unset __shcr_stamp
  else
    SHCR_SESSION_ID="${HOSTNAME:-host}-$$-${RANDOM}${RANDOM}"
  fi
  export SHCR_SESSION_ID
fi

# Probed once, because the fallback clock below runs on every command and
# checking the bash version there would cost more than it saves.
__SHCR_PRINTF_TIME=''
if printf -v __shcr_probe '%(%s)T' -1 2>/dev/null && [ -n "${__shcr_probe:-}" ]; then
  case "$__shcr_probe" in
    *[!0-9]* | '') ;;
    *) __SHCR_PRINTF_TIME=1 ;;
  esac
fi
unset __shcr_probe

__shcr_seq=0
__shcr_armed=''
__shcr_cmd_id=''
__shcr_bg=''
__shcr_last_hist=''
__shcr_ms=0

# Millisecond clock. Assigns to __shcr_ms rather than printing, because reading
# it back with $(...) would fork a subshell on every prompt — the two calls per
# command cost about as much as everything else here put together.
# EPOCHREALTIME honours the locale's decimal separator, hence the comma fixup.
__shcr_now_ms() {
  local t="${EPOCHREALTIME:-}"
  if [ -n "$t" ]; then
    t="${t/,/.}"
    local s="${t%%.*}" frac="${t#*.}000"
    __shcr_ms="${s}${frac:0:3}"
  else
    # bash before 5.0 has no EPOCHREALTIME. SECONDS is not a substitute: it
    # counts from when this shell started, so it would date every command to
    # somewhere just after 1970. Second precision is fine here; printf's
    # %(...)T needs bash 4.2, and older shells pay one fork.
    if [ -n "${__SHCR_PRINTF_TIME:-}" ]; then
      printf -v __shcr_ms '%(%s)T000' -1
    else
      __shcr_ms="$(date +%s)000"
    fi
  fi
}

# Fire and forget: the subshell wrapper keeps job-control chatter out of the
# user's terminal and returns as soon as the fork is done.
__shcr_send() {
  ( "$__SHCR_BIN" "$@" >/dev/null 2>&1 & ) 2>/dev/null
}

# The command text goes through the environment rather than the argument list.
# /proc/<pid>/cmdline is world-readable; /proc/<pid>/environ is not. Passing it
# as an argument would hand every recorded command to every other account on the
# machine, including builtins such as `export TOKEN=...` that never appear in
# the process list on their own.
__shcr_send_command() {
  ( SHCR_COMMAND="$1" "$__SHCR_BIN" "${@:2}" >/dev/null 2>&1 & ) 2>/dev/null
}

__shcr_preexec() {
  local bash_command="$1"
  local h hnum cmd trimmed
  # Read the line back from history rather than using $BASH_COMMAND: history has
  # the text as typed, without alias expansion and without splitting a pipeline
  # into its parts.
  h=$(HISTTIMEFORMAT='' builtin history 1 2>/dev/null) || return
  [ -z "$h" ] && return
  h="${h#"${h%%[![:space:]]*}"}"
  hnum="${h%%[[:space:]]*}"

  cmd="${h#*[[:space:]]}"
  cmd="${cmd#"${cmd%%[![:space:]]*}"}"
  [ -z "$cmd" ] && return

  if [ "$hnum" = "$__shcr_last_hist" ]; then
    # bash added no new history entry, and the two reasons need opposite
    # handling. Under HISTCONTROL=ignorespace a leading-space command was hidden
    # on purpose and must stay unrecorded; under ignoredups bash merely
    # collapsed a repeat, and that command really did run — dropping it would
    # lose every re-run of `make`, which for a history tool is unacceptable.
    # Debian's default is ignoreboth, so both are live at once for most users.
    #
    # What bash is about to execute separates the cases: for a collapsed repeat
    # it matches the history line, for a hidden command it does not. The stored
    # text always comes from history and never from $BASH_COMMAND, so a command
    # bash refused to record cannot leak in here even if this guess is wrong —
    # the worst case is a duplicate row of an already-visible command.
    case "$cmd" in
      "$bash_command" | "$bash_command "* | "$bash_command;"* | "$bash_command|"* | "$bash_command&"*) ;;
      *) return ;;
    esac
  fi
  __shcr_last_hist="$hnum"

  trimmed="${cmd%"${cmd##*[![:space:]]}"}"
  __shcr_bg=''
  case "$trimmed" in
    *'&&') ;;
    *'&') __shcr_bg='--background' ;;
  esac

  __shcr_seq=$((__shcr_seq + 1))
  __shcr_cmd_id="${SHCR_SESSION_ID}.${__shcr_seq}"

  __shcr_now_ms
  __shcr_send_command "$cmd" event start \
    --id "$__shcr_cmd_id" \
    --session "$SHCR_SESSION_ID" \
    --cwd "$PWD" \
    --shell bash \
    --pgid "$$" \
    --time "$__shcr_ms" \
    $__shcr_bg
}

__shcr_postexec() {
  local status="$1"
  [ -z "$__shcr_cmd_id" ] && return
  # A backgrounded command has not finished just because the prompt came back,
  # so there is no end event to send for it.
  if [ -z "$__shcr_bg" ]; then
    __shcr_now_ms
    __shcr_send event end \
      --id "$__shcr_cmd_id" \
      --exit "$status" \
      --time "$__shcr_ms"
  fi
  __shcr_cmd_id=''
  __shcr_bg=''
}

# Runs first in the prompt chain so it sees the real exit status before any
# other PROMPT_COMMAND entry clobbers $?.
__shcr_precmd() {
  local status=$?
  __shcr_armed=''
  if [ -n "$__shcr_cmd_id" ]; then
    __shcr_postexec "$status"
  fi
  return $status
}

# Runs last in the prompt chain: everything after this point is the user typing,
# so the DEBUG trap may fire again.
__shcr_arm() {
  __shcr_armed=1
}

# Preserve any DEBUG trap that was already installed (direnv, other history
# tools). Overwriting it is the classic way these tools break each other.
__shcr_prev_debug=''
__shcr_tmp="$(trap -p DEBUG)"
if [ -n "$__shcr_tmp" ]; then
  __shcr_prev_debug="${__shcr_tmp#*\'}"
  __shcr_prev_debug="${__shcr_prev_debug%\'*}"
fi
unset __shcr_tmp

__shcr_debug_trap() {
  local status=$?
  # The Ctrl+R widget is not a user command. bash does fire DEBUG for `bind -x`
  # commands, so without this the picker would both get recorded and consume the
  # arming that the command the user actually runs next needs.
  case "$BASH_COMMAND" in
    __shcr_pick*) return $status ;;
  esac
  if [ -n "${__shcr_in_pick:-}" ]; then
    return $status
  fi
  # Completion runs commands too; those are not user commands.
  if [ -z "${COMP_LINE:-}" ] && [ -n "$__shcr_armed" ]; then
    __shcr_armed=''
    __shcr_preexec "$BASH_COMMAND"
  fi
  if [ -n "$__shcr_prev_debug" ]; then
    eval "$__shcr_prev_debug"
  fi
  return $status
}

trap '__shcr_debug_trap' DEBUG

# Chain onto PROMPT_COMMAND without dropping what is already there. bash 5.1+
# allows an array, so handle both shapes.
if [[ "$(declare -p PROMPT_COMMAND 2>/dev/null)" == "declare -a"* ]]; then
  PROMPT_COMMAND=(__shcr_precmd "${PROMPT_COMMAND[@]}" __shcr_arm)
else
  # shellcheck disable=SC2178,SC2128  # reached only when PROMPT_COMMAND is a string
  PROMPT_COMMAND="__shcr_precmd${PROMPT_COMMAND:+;$PROMPT_COMMAND};__shcr_arm"
fi

# ---------------------------------------------------------------- Ctrl+R

# Puts the chosen command in the prompt, editable and unexecuted. The picker
# writes its interface to /dev/tty and only the selection to stdout, so this
# capture gets the command and nothing else.
__shcr_pick() {
  __shcr_in_pick=1
  local out
  out="$("$__SHCR_BIN" tui --query "$READLINE_LINE" < /dev/tty)"
  if [ -n "$out" ]; then
    READLINE_LINE="$out"
    READLINE_POINT=${#READLINE_LINE}
  fi
  __shcr_in_pick=''
}

if [ -z "${SHCR_NO_BIND:-}" ]; then
  bind -x '"\C-r": __shcr_pick' 2>/dev/null
fi

# ---------------------------------------------------------------- sync hints
#
# Sitting down at a terminal is the moment other machines' history is most
# worth having, and closing one is the last chance to push what just happened.
# Both are cheap: one backgrounded process, nothing on the prompt path.

__shcr_nudge() {
  ( "$__SHCR_BIN" nudge "$1" >/dev/null 2>&1 & ) 2>/dev/null
}

# Preserve any EXIT trap already installed rather than replacing it — the same
# courtesy the DEBUG trap gets, and for the same reason.
__shcr_prev_exit=''
__shcr_tmp="$(trap -p EXIT)"
if [ -n "$__shcr_tmp" ]; then
  __shcr_prev_exit="${__shcr_tmp#*\'}"
  __shcr_prev_exit="${__shcr_prev_exit%\'*}"
fi
unset __shcr_tmp

__shcr_on_exit() {
  __shcr_nudge session-end
  if [ -n "$__shcr_prev_exit" ]; then
    eval "$__shcr_prev_exit"
  fi
}
trap '__shcr_on_exit' EXIT

__shcr_nudge session-start
