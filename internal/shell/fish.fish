# shellcrumbs — fish integration
# shcr init fish | source

if set -q __SHCR_LOADED
    # return, not exit: this file is sourced, so exit would close the shell.
    return 0
end
set -g __SHCR_LOADED 1
set -g __SHCR_BIN '@SHCR_BIN@'

if not set -q SHCR_SESSION_ID
    set -gx SHCR_SESSION_ID (hostname)-$fish_pid-(date +%s%N | string sub -l 13)
end

set -g __shcr_seq 0
set -g __shcr_cmd_id ''
set -g __shcr_bg ''

function __shcr_now_ms
    date +%s%3N
end

function __shcr_preexec --on-event fish_preexec
    set -l cmd $argv[1]
    test -z "$cmd"; and return
    # fish does not save a leading-space command to its own history, so
    # recording one here would put it somewhere the user expects it not to be.
    string match -q ' *' -- "$cmd"; and return

    set -l trimmed (string trim -- "$cmd")
    set -g __shcr_bg ''
    if string match -qr '(?<!&)&$' -- "$trimmed"
        set -g __shcr_bg '--background'
    end

    set -g __shcr_seq (math $__shcr_seq + 1)
    set -g __shcr_cmd_id "$SHCR_SESSION_ID.$__shcr_seq"

    # Command text goes through the environment, not the argument list:
    # /proc/<pid>/cmdline is world-readable and /proc/<pid>/environ is not.
    env SHCR_COMMAND="$cmd" $__SHCR_BIN event start \
        --id "$__shcr_cmd_id" \
        --session "$SHCR_SESSION_ID" \
        --cwd "$PWD" \
        --shell fish \
        --pgid $fish_pid \
        --time (__shcr_now_ms) \
        $__shcr_bg >/dev/null 2>&1 &
    disown 2>/dev/null
end

function __shcr_postexec --on-event fish_postexec
    set -l status_code $status
    test -z "$__shcr_cmd_id"; and return
    # A backgrounded command has not finished just because the prompt is back.
    if test -z "$__shcr_bg"
        $__SHCR_BIN event end \
            --id "$__shcr_cmd_id" \
            --exit $status_code \
            --time (__shcr_now_ms) >/dev/null 2>&1 &
        disown 2>/dev/null
    end
    set -g __shcr_cmd_id ''
    set -g __shcr_bg ''
end

# ---------------------------------------------------------------- Ctrl+R

function __shcr_pick
    # string collect keeps the buffer as a single argument. Command substitution
    # splits on newlines, so a multi-line buffer would arrive as several
    # arguments, and an empty one would expand to nothing at all — leaving
    # --query with no value to consume and the picker refusing to start.
    set -l q (commandline | string collect --allow-empty)
    set -l out ($__SHCR_BIN tui --query=$q </dev/tty)
    if test -n "$out"
        commandline -r -- $out
    end
    commandline -f repaint
end

if not set -q SHCR_NO_BIND
    bind \cr __shcr_pick
end

# ---------------------------------------------------------------- sync hints

function __shcr_nudge
    fish -c "$__SHCR_BIN nudge $argv[1] >/dev/null 2>&1 &" >/dev/null 2>&1
end

function __shcr_on_exit --on-event fish_exit
    __shcr_nudge session-end
end

__shcr_nudge session-start
