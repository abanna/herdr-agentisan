#!/usr/bin/env bash
# shift.sh: the boss's shift handoff, by hand, until `boss handoff` exists in
# the plugin (ADR-001 item 6, NERD-5262). Agreed with the user 2026-11-22:
# no compaction, no summary, no blind handoff. A fresh boss starts in a new
# pane, reads the record and the live state itself, and the OUTGOING boss
# checks it has the picture before the old pane closes ("like a shift worker").
#
# Nothing here types into the outgoing boss's pane: the user may be typing
# there. The incoming boss talks to the outgoing one through files only.
#
#   outgoing boss:  shift.sh open            split a pane beside me, start the incoming boss
#                   shift.sh brief           send the incoming boss its first instructions
#                   shift.sh wait [N]        block until the incoming boss writes account-N.md (default: next)
#                   shift.sh say "<text>"    tell the incoming boss something (typed into ITS pane)
#                   shift.sh go              write GO: the incoming boss may take over
#                   shift.sh abort "<why>"   close the incoming pane; I stay in charge
#   incoming boss:  shift.sh takeover        after GO: close the old pane, take the name
#   either:         shift.sh status
set -euo pipefail
BOSS=${SHIFT_BOSS:-ha-boss}
NEXT=${SHIFT_NEXT:-${BOSS}-next}
REPO=${SHIFT_REPO:-/home/abanna/Development/personal/github/abanna/herdr-agentisan}
ROOT=${SHIFT_ROOT:-$HOME/.local/share/ha-team/boss-shift}
CUR=$ROOT/current
# The boss runs on Fable (the user, 2026-11-23: "the boss should be fable").
# The first two shifts passed no model, so the incoming boss got whatever
# Claude Code's default was that day: on 2026-11-23 that was Sonnet.
MODEL=${SHIFT_MODEL:-fable}
EFFORT=${SHIFT_EFFORT:-high}
here=$(cd "$(dirname "$0")" && pwd)
die(){ echo "shift: $*" >&2; exit 1; }
field(){ python3 -c 'import json,sys
d=json.load(sys.stdin)
for k in sys.argv[1].split("."):
    d=d.get(k) if isinstance(d,dict) else None
print("" if d is None else d)' "$1"; }
need_cur(){ [ -L "$CUR" ] && [ -d "$CUR" ] || die "no shift is open (run: shift.sh open)"; }
val(){ sed -n "s/^$1=//p" "$CUR/state" | tail -1; }
# model_of <agent>: the model that agent's own statusline reports on its pane
# (the plugin's `model` token, ADR-001 A56); empty when it has reported none.
model_of(){ { herdr agent get "$1" 2>/dev/null | field result.agent.tokens.model 2>/dev/null; } || true; }
# model_ok <reported> <wanted>: an alias ("fable") matches a full id ("claude-fable-5-2").
model_ok(){ case "$(printf %s "$1" | tr '[:upper:]' '[:lower:]')" in *"$(printf %s "$2" | tr '[:upper:]' '[:lower:]')"*) return 0 ;; esac; return 1; }

cmd=${1:-status}; shift || true
case "$cmd" in
  open)
    [ -L "$CUR" ] && die "a shift is already open at $(readlink "$CUR"); finish or abort it first"
    old=${HERDR_PANE_ID:-}; [ -n "$old" ] || die "HERDR_PANE_ID is not set: run this from the outgoing boss's own pane"
    who=$(herdr agent get "$BOSS" | field result.agent.pane_id)
    [ "$who" = "$old" ] || die "agent $BOSS is on pane '$who', and I am on '$old': refusing"
    dir=$ROOT/$(date +%Y%m%d-%H%M%S); mkdir -p "$dir"
    new=$(herdr pane split "$old" --direction right --no-focus --cwd "$REPO" | field result.pane.pane_id)
    [ -n "$new" ] || die "the split returned no pane id"
    printf 'old=%s\nnew=%s\nboss=%s\nnext=%s\nopened=%s\nmodel=%s\neffort=%s\n' "$old" "$new" "$BOSS" "$NEXT" "$(date +%H:%M:%S)" "$MODEL" "$EFFORT" > "$dir/state"
    ln -s "$dir" "$CUR"
    # the pane needs a moment to reach its shell prompt
    herdr agent start "$NEXT" --kind claude --pane "$new" --timeout 90000 -- --name "$NEXT" --model "$MODEL" --effort "$EFFORT" >/dev/null \
      || die "the incoming boss did not start in $new (pane left open; run: shift.sh abort)"
    # what it was asked for is not what it runs: read the model back from its pane
    got=; for _ in $(seq 1 30); do got=$(model_of "$NEXT"); [ -n "$got" ] && break; sleep 2; done
    if [ -z "$got" ]; then
      echo "warning: $NEXT has reported no model yet (asked for $MODEL); 'shift.sh go' checks again and refuses a wrong one"
    elif ! model_ok "$got" "$MODEL"; then
      die "the incoming boss runs on '$got', not '$MODEL' (pane $new left open; run: shift.sh abort, then open again)"
    fi
    echo "opened: incoming boss $NEXT on $new, model ${got:-not reported yet}; shift folder $dir" ;;
  brief)
    need_cur
    text="You are the INCOMING boss of the herdr-agentisan team, starting a shift change. The outgoing boss is still in charge in the pane beside you. Read $here/INCOMING.md now and follow it exactly. Your shift folder is $(readlink "$CUR"). Do not message workers, do not run 'herdr-agentisan board read', and never type into the other pane."
    herdr agent prompt "$(val next)" "$text" >/dev/null && echo "briefed $(val next)" ;;
  wait)
    need_cur
    n=${1:-}; if [ -z "$n" ]; then n=1; while [ -e "$CUR/account-$n.md" ] && [ -e "$CUR/reply-$n.md" ]; do n=$((n+1)); done; fi
    until [ -s "$CUR/account-$n.md" ] || [ -e "$CUR/ABORTED" ]; do sleep 5; done
    [ -e "$CUR/ABORTED" ] && { echo aborted; exit 0; }
    echo "$CUR/account-$n.md is in" ;;
  say)
    need_cur; [ $# -ge 1 ] || die "say what?"
    herdr agent prompt "$(val next)" "From the outgoing boss: $*" >/dev/null && echo said ;;
  go)
    need_cur
    [ "${HERDR_PANE_ID:-}" = "$(val old)" ] || die "only the outgoing boss (pane $(val old)) writes GO"
    ls "$CUR"/account-*.md >/dev/null 2>&1 || die "the incoming boss has written no account yet"
    # no GO to a boss on the wrong model: the team is not handed to it
    want=$(val model); want=${want:-$MODEL}; got=$(model_of "$(val next)")
    if [ -z "$got" ]; then
      [ -n "${SHIFT_MODEL_SEEN:-}" ] || die "the incoming boss's pane reports no model, so I cannot check it is on '$want'. Look yourself ('herdr agent read $(val next)' shows the model under its prompt); if it is right, run: SHIFT_MODEL_SEEN=1 shift.sh go. GO not written."
    elif ! model_ok "$got" "$want"; then
      die "the incoming boss runs on '$got'; the boss must run on '$want'. Ask the user to switch it with /model in pane $(val new), or abort and open again. GO not written."
    fi
    date +%H:%M:%S > "$CUR/GO"
    herdr agent prompt "$(val next)" "From the outgoing boss: GO. You have the picture. Run: $here/shift.sh takeover" >/dev/null
    echo "GO written; the incoming boss takes over now. Stop your board wait and timers and write nothing more." ;;
  abort)
    need_cur
    echo "${*:-no reason given}" > "$CUR/ABORTED"
    herdr pane close "$(val new)" >/dev/null 2>&1 || true
    mv "$CUR" "$ROOT/last-aborted" 2>/dev/null || rm -f "$CUR"
    echo "aborted: incoming pane closed; the outgoing boss stays in charge. Tell the user." ;;
  takeover)
    need_cur
    me=${HERDR_PANE_ID:-}; [ "$me" = "$(val new)" ] || die "only the incoming boss (pane $(val new)) takes over; I am '$me'"
    [ -s "$CUR/GO" ] || die "no GO from the outgoing boss yet"
    old=$(val old); boss=$(val boss)
    herdr pane close "$old" >/dev/null || die "could not close the old pane $old; nothing renamed"
    # the name moves only after the old pane is gone (ADR-001 A7)
    for i in 1 2 3 4 5; do herdr agent get "$boss" >/dev/null 2>&1 || break; sleep 1; done
    herdr agent rename "$(val next)" "$boss" >/dev/null || die "old pane closed but the rename failed: run 'herdr agent rename $(val next) $boss' by hand NOW"
    date +%H:%M:%S > "$CUR/DONE"
    d=$(readlink "$CUR"); rm -f "$CUR"
    echo "took over as $boss on $me (shift folder $d). Now: read the board, start ONE board wait, the inbox watcher and the timer, renew the mutes, and tell the user in two lines." ;;
  status)
    if [ -L "$CUR" ]; then cat "$CUR/state"; echo "reported_model=$(model_of "$(val next)")"; ls "$CUR"; else echo "no shift open"; fi ;;
  *) die "unknown command '$cmd'" ;;
esac
