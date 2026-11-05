package execsandbox

// execScript runs a command so that it cannot outlive whoever is waiting for it.
//
// Two watchers kill the process group, which every exec'd process leads: one when
// the timeout passes, and one when stdin closes because the caller cancelled or
// died. The timer still works if the stream never reports the caller's death.
// Both are stopped when the command ends, so a background job it started survives
// that command as it would on any machine. stdin is reached through fd 3 because
// a background list without job control gets /dev/null for stdin. The command is
// exec'd in a subshell so that its name can never resolve to a shell builtin.
// stderr closes before the watchers stop, because bash reports a killed job there.
// The last thing printed is the exit marker, which says the command has ended.
const execScript = `workdir=$1 limit=$2 activity=$3 marker=$4
shift 4
touch "$activity" 2>/dev/null
cd "$workdir" || exit 1
exec 3<&0
(
	cat <&3 >/dev/null
	kill -s KILL -- "-$$"
) >/dev/null 2>&1 &
stream=$!
(
	while [ "$limit" -gt 0 ]; do
		sleep 1
		limit=$((limit - 1))
	done
	kill -s KILL -- "-$$"
) </dev/null >/dev/null 2>&1 &
timer=$!
( exec "$@" ) </dev/null 3<&-
code=$?
exec 2>/dev/null
kill "$stream" "$timer"
touch "$activity"
printf '%s%d;' "$marker" "$code"
exit "$code"
`

const touchScript = `touch "$1"
`
