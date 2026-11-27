# Shared by the fakes. They live in <fake root>/.fakes and keep state in .fakes/state.
root=$(cd "$(dirname "$0")/.." && pwd)
st=$root/.fakes/state
mkdir -p "$st"
log() { [ -z "${FAKE_LOG:-}" ] || printf '%s\n' "$*" >>"$FAKE_LOG"; }
put() { [ "$(cat "$st/$1" 2>/dev/null)" = "$2" ] || printf '%s\n' "$2" >"$st/$1"; }
