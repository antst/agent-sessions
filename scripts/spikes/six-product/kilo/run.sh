#!/usr/bin/env bash
set -eu

# Real-product Phase-0 spike for Kilo 7.5.6. The Kilo server/TUI/protocol are
# never mocked; only the OpenAI-compatible model behind Kilo is deterministic.

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
REPO_ROOT=$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel)
PINNED_KILO_VERSION=7.5.6
KEEP=${KILO_S1_KEEP:-0}

for command_name in curl git jq node npm tmux; do
  command -v "$command_name" >/dev/null 2>&1 || {
    printf 'missing required command: %s\n' "$command_name" >&2
    exit 2
  }
done

scratch=$(mktemp -d "${TMPDIR:-/tmp}/agent-sessions-kilo-s1.XXXXXX")
chmod 700 "$scratch"
tmux_name="kilo-s1-$$"
mock_pid=
server_a_pid=
server_b_pid=
event_a_pid=
event_b_pid=
pass_a=$(node -e 'process.stdout.write(require("node:crypto").randomBytes(24).toString("hex"))')
pass_b=$(node -e 'process.stdout.write(require("node:crypto").randomBytes(24).toString("hex"))')
username=kilo
background_id=

stop_pid() {
  target_pid=$1
  case "$target_pid" in
    ''|*[!0-9]*) return 0 ;;
  esac
  kill "$target_pid" >/dev/null 2>&1 || true
  wait "$target_pid" >/dev/null 2>&1 || true
}

cleanup() {
  status=$?
  trap - EXIT HUP INT TERM
  if [ -n "$background_id" ] && [ -n "${url_b:-}" ]; then
    curl -fsS -u "$username:$pass_b" -X POST \
      --get --data-urlencode "directory=${project_b:-}" \
      "$url_b/background-process/$background_id/stop" >/dev/null 2>&1 || true
  fi
  tmux -L "$tmux_name" kill-server >/dev/null 2>&1 || true
  stop_pid "$event_a_pid"
  stop_pid "$event_b_pid"
  stop_pid "$server_a_pid"
  stop_pid "$server_b_pid"
  stop_pid "$mock_pid"
  if [ "$KEEP" = 1 ] || [ "$status" -ne 0 ]; then
    printf 'Kilo S1 scratch retained at %s\n' "$scratch" >&2
  else
    case "$scratch" in
      "${TMPDIR:-/tmp}"/agent-sessions-kilo-s1.*)
        if [ -d "$scratch" ] && [ ! -L "$scratch" ]; then
          rm -rf -- "$scratch"
        fi
        ;;
      *) printf 'refusing to remove unexpected scratch path: %s\n' "$scratch" >&2 ;;
    esac
  fi
  exit "$status"
}
trap cleanup EXIT HUP INT TERM

fail() {
  printf 'S1 RED: %s\n' "$*" >&2
  exit 1
}

wait_for() {
  description=$1
  shift
  tries=0
  while [ "$tries" -lt 120 ]; do
    if "$@" >/dev/null 2>&1; then return 0; fi
    tries=$((tries + 1))
    sleep 0.25
  done
  fail "timed out waiting for $description"
}

api_get() {
  base=$1 password=$2 path=$3 directory=$4
  curl -fsS -u "$username:$password" --get --data-urlencode "directory=$directory" "$base$path"
}

api_post() {
  base=$1 password=$2 path=$3 directory=$4 body=${5-}
  encoded_directory=$(printf '%s' "$directory" | jq -sRr @uri)
  if [ -n "$body" ]; then
    curl -fsS -u "$username:$password" -X POST -H 'content-type: application/json' \
      --data "$body" "$base$path?directory=$encoded_directory"
  else
    curl -fsS -u "$username:$password" -X POST "$base$path?directory=$encoded_directory"
  fi
}

api_patch() {
  base=$1 password=$2 path=$3 directory=$4 body=$5
  encoded_directory=$(printf '%s' "$directory" | jq -sRr @uri)
  curl -fsS -u "$username:$password" -X PATCH -H 'content-type: application/json' \
    --data "$body" "$base$path?directory=$encoded_directory"
}

server_ready() {
  curl -fsS -u "$1:$2" "$3/doc" >/dev/null
}

session_count_positive() {
  api_get "$1" "$2" /session "$3" | jq -e 'length > 0' >/dev/null
}

session_messages() {
  api_get "$1" "$2" "/session/$4/message" "$3"
}

message_contains() {
  session_messages "$1" "$2" "$3" "$4" |
    jq -e --arg marker "$5" '[.[] | .parts[]? | select(.type == "text" and ((.text // "") | contains($marker)))] | length > 0' >/dev/null
}

message_count() {
  session_messages "$1" "$2" "$3" "$4" |
    jq --arg marker "$5" '[.[] | .parts[]? | select(.type == "text" and ((.text // "") | contains($marker)))] | length'
}

session_busy() {
  api_get "$1" "$2" /session/status "$3" |
    jq -e --arg session "$4" '.[ $session ].type == "busy"' >/dev/null
}

session_idle() {
  status_json=$(api_get "$1" "$2" /session/status "$3")
  printf '%s' "$status_json" | jq -e --arg session "$4" 'if has($session) then .[$session].type == "idle" else true end' >/dev/null
}

session_title_is() {
  api_get "$1" "$2" "/session/$4" "$3" | jq -e --arg title "$5" '.title == $title' >/dev/null
}

capture_tui() {
  tmux -L "$tmux_name" capture-pane -p -J -S -1000 -t "$1"
}

tui_contains() {
  capture_tui "$1" | grep -F "$2" >/dev/null
}

select_spike_model() {
  tui=$1
  tmux -L "$tmux_name" send-keys -t "$tui" C-x m
  sleep 0.8
  tmux -L "$tmux_name" send-keys -t "$tui" -l 'Agent Sessions Spike'
  sleep 1
  tmux -L "$tmux_name" send-keys -t "$tui" Enter
  sleep 1
}

send_tui_prompt() {
  base=$1 password=$2 directory=$3 marker=$4
  api_post "$base" "$password" /tui/clear-prompt "$directory" >/dev/null
  body=$(jq -cn --arg text "$marker" '{text:$text}')
  api_post "$base" "$password" /tui/append-prompt "$directory" "$body" >/dev/null
}

submit_tui_prompt() {
  api_post "$1" "$2" /tui/submit-prompt "$3" >/dev/null
}

start_attach() {
  tui=$1 base=$2 password_var=$3 home=$4 data=$5 state=$6 cache=$7 config=$8 project=$9
  shift 9
  pass_name=$password_var
  tmux -L "$tmux_name" set-environment -g "$pass_name" "$(eval "printf '%s' \"\${$pass_name}\"")"
  command_line="exec env -i HOME='$home' XDG_DATA_HOME='$data' XDG_STATE_HOME='$state' XDG_CACHE_HOME='$cache' XDG_CONFIG_HOME='$config' PATH='$runtime_path' TERM=xterm-256color COLORTERM=truecolor LANG=C.UTF-8 KILO_SERVER_USERNAME='$username' KILO_SERVER_PASSWORD=\"\$$pass_name\" '$kilo' attach '$base' --dir '$project'"
  while [ "$#" -gt 0 ]; do
    command_line="$command_line '$1'"
    shift
  done
  tmux -L "$tmux_name" new-session -d -s "$tui" -x 160 -y 45 "$command_line"
}

printf 'Installing real Kilo CLI %s into isolated prefix...\n' "$PINNED_KILO_VERSION"
mkdir -p "$scratch/prefix"
npm install --silent --no-audit --no-fund --prefix "$scratch/prefix" \
  "@kilocode/cli@$PINNED_KILO_VERSION"
kilo="$scratch/prefix/node_modules/.bin/kilo"
[ -x "$kilo" ] || fail 'isolated Kilo executable missing'
actual_version=$($kilo --version 2>/dev/null | tail -n 1)
[ "$actual_version" = "$PINNED_KILO_VERSION" ] || fail "Kilo version $actual_version != $PINNED_KILO_VERSION"

node_bin=$(command -v node)
runtime_path=$(dirname "$node_bin"):/usr/local/bin:/usr/bin:/bin
mock_log="$scratch/mock.log"
schema_log="$scratch/tool-schemas.log"
KILO_SPIKE_MOCK_PORT=0 KILO_SPIKE_MOCK_LOG="$mock_log" KILO_SPIKE_SCHEMA_LOG="$schema_log" \
  "$node_bin" "$SCRIPT_DIR/mock-openai.mjs" >"$scratch/mock.stdout" 2>"$scratch/mock.stderr" &
mock_pid=$!
wait_for 'mock model listener' grep -q 'mock-openai listening' "$scratch/mock.stdout"
mock_port=$(sed -n 's#.*127\.0\.0\.1:\([0-9][0-9]*\).*#\1#p' "$scratch/mock.stdout" | head -n 1)
case "$mock_port" in ''|*[!0-9]*) fail 'could not parse mock-model port' ;; esac

for suffix in a b; do
  eval "home_$suffix='$scratch/home-$suffix'"
  eval "data_$suffix='$scratch/xdg-$suffix/data'"
  eval "state_$suffix='$scratch/xdg-$suffix/state'"
  eval "cache_$suffix='$scratch/xdg-$suffix/cache'"
  eval "config_$suffix='$scratch/xdg-$suffix/config'"
  eval "project_$suffix='$scratch/project-$suffix'"
done
mkdir -p "$home_a" "$data_a" "$state_a" "$cache_a" "$config_a/kilo" "$project_a"
mkdir -p "$home_b" "$data_b" "$state_b" "$cache_b" "$config_b/kilo" "$project_b"

write_config() {
  path=$1
  jq -n --arg base "http://127.0.0.1:$mock_port/v1" '{
    "$schema":"https://app.kilo.ai/config.json",
    model:"spike/spike",
    permission:{bash:"allow",background_process:"allow",external_directory:"allow"},
    provider:{spike:{
      npm:"@ai-sdk/openai-compatible",
      models:{spike:{name:"Agent Sessions Spike",limit:{context:32768,output:4096}}},
      options:{apiKey:"none",baseURL:$base}
    }}
  }' >"$path"
  chmod 600 "$path"
}
write_config "$config_a/kilo/kilo.json"
write_config "$config_b/kilo/kilo.json"

printf '{"private":true}\n' >"$project_a/package.json"
printf '{"private":true}\n' >"$project_b/package.json"

port_a=$($node_bin "$SCRIPT_DIR/free-port.mjs")
port_b=$($node_bin "$SCRIPT_DIR/free-port.mjs")
[ "$port_a" != "$port_b" ] || fail 'ephemeral server ports collided'
url_a="http://127.0.0.1:$port_a"
url_b="http://127.0.0.1:$port_b"

(
  cd "$project_a"
  exec env -i HOME="$home_a" XDG_DATA_HOME="$data_a" XDG_STATE_HOME="$state_a" \
    XDG_CACHE_HOME="$cache_a" XDG_CONFIG_HOME="$config_a" PATH="$runtime_path" \
    KILO_SERVER_USERNAME="$username" KILO_SERVER_PASSWORD="$pass_a" \
    TERM=xterm-256color LANG=C.UTF-8 \
    "$kilo" serve --hostname 127.0.0.1 --port "$port_a"
) >"$scratch/server-a.log" 2>&1 &
server_a_pid=$!
(
  cd "$project_b"
  exec env -i HOME="$home_b" XDG_DATA_HOME="$data_b" XDG_STATE_HOME="$state_b" \
    XDG_CACHE_HOME="$cache_b" XDG_CONFIG_HOME="$config_b" PATH="$runtime_path" \
    KILO_SERVER_USERNAME="$username" KILO_SERVER_PASSWORD="$pass_b" \
    TERM=xterm-256color LANG=C.UTF-8 \
    "$kilo" serve --hostname 127.0.0.1 --port "$port_b"
) >"$scratch/server-b.log" 2>&1 &
server_b_pid=$!
wait_for 'Kilo server A' server_ready "$username" "$pass_a" "$url_a"
wait_for 'Kilo server B' server_ready "$username" "$pass_b" "$url_b"

# Keep the isolated tmux server alive before installing its in-memory launch
# secrets; an empty tmux server exits immediately.
tmux -L "$tmux_name" new-session -d -s keeper 'sleep 86400'
PASS_A=$pass_a
PASS_B=$pass_b
session_a=$(api_post "$url_a" "$pass_a" /session "$project_a" '{"title":"S1_PAIR_A","model":{"id":"spike","providerID":"spike"}}' | jq -r .id)
session_b=$(api_post "$url_b" "$pass_b" /session "$project_b" '{"title":"S1_PAIR_B","model":{"id":"spike","providerID":"spike"}}' | jq -r .id)
case "$session_a:$session_b" in ses_*:ses_*) ;; *) fail 'attached TUIs did not publish native session IDs' ;; esac
[ "$session_a" != "$session_b" ] || fail 'isolated attach pairs reused one native session ID'
start_attach attach-a "$url_a" PASS_A "$home_a" "$data_a" "$state_a" "$cache_a" "$config_a" "$project_a" --session "$session_a"
start_attach attach-b "$url_b" PASS_B "$home_b" "$data_b" "$state_b" "$cache_b" "$config_b" "$project_b" --session "$session_b"
wait_for 'attached TUI A binding' tui_contains attach-a S1_PAIR_A
wait_for 'attached TUI B binding' tui_contains attach-b S1_PAIR_B

select_spike_model attach-a
select_spike_model attach-b

send_tui_prompt "$url_a" "$pass_a" "$project_a" ROUTE_A_EXACT
send_tui_prompt "$url_b" "$pass_b" "$project_b" ROUTE_B_EXACT
sleep 0.5
screen_a=$(capture_tui attach-a)
screen_b=$(capture_tui attach-b)
printf '%s' "$screen_a" | grep -F ROUTE_A_EXACT >/dev/null || fail 'route A marker was not rendered by attached TUI A'
printf '%s' "$screen_a" | grep -F ROUTE_B_EXACT >/dev/null && fail 'route B marker crossed into attached TUI A'
printf '%s' "$screen_b" | grep -F ROUTE_B_EXACT >/dev/null || fail 'route B marker was not rendered by attached TUI B'
printf '%s' "$screen_b" | grep -F ROUTE_A_EXACT >/dev/null && fail 'route A marker crossed into attached TUI B'

curl -NsS --max-time 180 -u "$username:$pass_a" --get --data-urlencode "directory=$project_a" \
  "$url_a/event" >"$scratch/events-a.sse" 2>"$scratch/events-a.stderr" &
event_a_pid=$!
curl -NsS --max-time 180 -u "$username:$pass_b" --get --data-urlencode "directory=$project_b" \
  "$url_b/event" >"$scratch/events-b.sse" 2>"$scratch/events-b.stderr" &
event_b_pid=$!
sleep 0.5

send_tui_prompt "$url_a" "$pass_a" "$project_a" ROUTE_A_MODEL
submit_tui_prompt "$url_a" "$pass_a" "$project_a"
send_tui_prompt "$url_b" "$pass_b" "$project_b" ROUTE_B_MODEL
submit_tui_prompt "$url_b" "$pass_b" "$project_b"
wait_for 'route A model completion' message_contains "$url_a" "$pass_a" "$project_a" "$session_a" 'MOCK_OK:ROUTE_A_MODEL'
wait_for 'route B model completion' message_contains "$url_b" "$pass_b" "$project_b" "$session_b" 'MOCK_OK:ROUTE_B_MODEL'
[ "$(message_count "$url_a" "$pass_a" "$project_a" "$session_a" ROUTE_B_MODEL)" -eq 0 ] || fail 'route B model input crossed into server A session'
[ "$(message_count "$url_b" "$pass_b" "$project_b" "$session_b" ROUTE_A_MODEL)" -eq 0 ] || fail 'route A model input crossed into server B session'

send_tui_prompt "$url_b" "$pass_b" "$project_b" IDLE_WAKE_VIA_ATTACH
submit_tui_prompt "$url_b" "$pass_b" "$project_b"
wait_for 'idle-wake completion' message_contains "$url_b" "$pass_b" "$project_b" "$session_b" 'MOCK_OK:IDLE_WAKE_VIA_ATTACH'

send_tui_prompt "$url_b" "$pass_b" "$project_b" SLOW_TURN_FIRST
submit_tui_prompt "$url_b" "$pass_b" "$project_b"
wait_for 'busy state for first turn' session_busy "$url_b" "$pass_b" "$project_b" "$session_b"
send_tui_prompt "$url_b" "$pass_b" "$project_b" BUSY_QUEUE_SECOND
submit_tui_prompt "$url_b" "$pass_b" "$project_b"
wait_for 'queued second turn completion' message_contains "$url_b" "$pass_b" "$project_b" "$session_b" 'MOCK_OK:BUSY_QUEUE_SECOND'
wait_for 'final idle state' session_idle "$url_b" "$pass_b" "$project_b" "$session_b"
user_order=$(session_messages "$url_b" "$pass_b" "$project_b" "$session_b" |
  jq -c '[.[] | select(.info.role == "user") | .parts[]? | select(.type == "text") | .text | select(. == "SLOW_TURN_FIRST" or . == "BUSY_QUEUE_SECOND")]')
[ "$user_order" = '["SLOW_TURN_FIRST","BUSY_QUEUE_SECOND"]' ] || fail "busy queue order was $user_order"

mcp_body=$(jq -cn --arg node "$node_bin" --arg script "$SCRIPT_DIR/mock-mcp.mjs" \
  '{name:"s1-mcp",config:{type:"local",command:[$node,$script],enabled:true,timeout:5000}}')
mcp_a=$(api_post "$url_a" "$pass_a" /mcp "$project_a" "$mcp_body")
mcp_b=$(api_post "$url_b" "$pass_b" /mcp "$project_b" "$mcp_body")
printf '%s' "$mcp_a" | jq -e '.["s1-mcp"].status == "connected"' >/dev/null || fail 'MCP unavailable in attach pair A'
printf '%s' "$mcp_b" | jq -e '.["s1-mcp"].status == "connected"' >/dev/null || fail 'MCP unavailable in attach pair B'

send_tui_prompt "$url_b" "$pass_b" "$project_b" BACKGROUND_ATTRIBUTION
submit_tui_prompt "$url_b" "$pass_b" "$project_b"
background_ready() {
  api_get "$url_b" "$pass_b" /background-process "$project_b" |
    jq -e --arg session "$session_b" --arg cwd "$project_b" \
      '.[] | select(.sessionID == $session and .cwd == $cwd and (.pid // 0) > 0 and (.status == "ready" or .status == "running"))' >/dev/null
}
wait_for 'background-process attribution' background_ready
background_json=$(api_get "$url_b" "$pass_b" /background-process "$project_b")
background_id=$(printf '%s' "$background_json" | jq -r --arg session "$session_b" '.[] | select(.sessionID == $session) | .id' | head -n 1)
case "$background_id" in bgp_*) ;; *) fail 'background process had no native bgp ID' ;; esac

api_patch "$url_a" "$pass_a" "/session/$session_a" "$project_a" '{"title":"S1_RENAMED_A"}' >/dev/null
api_patch "$url_b" "$pass_b" "/session/$session_b" "$project_b" '{"title":"S1_RENAMED_B"}' >/dev/null
wait_for 'rename A persistence' session_title_is "$url_a" "$pass_a" "$project_a" "$session_a" S1_RENAMED_A
wait_for 'rename B persistence' session_title_is "$url_b" "$pass_b" "$project_b" "$session_b" S1_RENAMED_B
wait_for 'renamed title in TUI A' tui_contains attach-a S1_RENAMED_A
wait_for 'renamed title in TUI B' tui_contains attach-b S1_RENAMED_B

tmux -L "$tmux_name" kill-session -t attach-a
tmux -L "$tmux_name" kill-session -t attach-b
start_attach resume-a "$url_a" PASS_A "$home_a" "$data_a" "$state_a" "$cache_a" "$config_a" "$project_a" --session "$session_a"
start_attach resume-b "$url_b" PASS_B "$home_b" "$data_b" "$state_b" "$cache_b" "$config_b" "$project_b" --session "$session_b"
wait_for 'resumed title A' tui_contains resume-a S1_RENAMED_A
wait_for 'resumed title B' tui_contains resume-b S1_RENAMED_B
send_tui_prompt "$url_a" "$pass_a" "$project_a" RESUME_SAME_NATIVE_A
submit_tui_prompt "$url_a" "$pass_a" "$project_a"
wait_for 'same-session resume completion' message_contains "$url_a" "$pass_a" "$project_a" "$session_a" 'MOCK_OK:RESUME_SAME_NATIVE_A'

wait_for 'busy completion event A' grep -F "\"sessionID\":\"$session_a\",\"status\":{\"type\":\"busy\"}" "$scratch/events-a.sse"
wait_for 'idle completion event A' grep -F "\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"$session_a\"}" "$scratch/events-a.sse"
wait_for 'busy completion event B' grep -F "\"sessionID\":\"$session_b\",\"status\":{\"type\":\"busy\"}" "$scratch/events-b.sse"
wait_for 'idle completion event B' grep -F "\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"$session_b\"}" "$scratch/events-b.sse"

# Kilo 7.5.6's --mini client is intentionally checked as a negative. It can
# resume/read the session, but the peer-driving /tui/* routes target the full
# TUI controller and do not submit through mini.
tmux -L "$tmux_name" kill-session -t resume-a
before_mini=$(message_count "$url_a" "$pass_a" "$project_a" "$session_a" MINI_TUI_ROUTE_PROBE)
start_attach mini-a "$url_a" PASS_A "$home_a" "$data_a" "$state_a" "$cache_a" "$config_a" "$project_a" --session "$session_a" --mini
sleep 1
send_tui_prompt "$url_a" "$pass_a" "$project_a" MINI_TUI_ROUTE_PROBE
submit_tui_prompt "$url_a" "$pass_a" "$project_a"
sleep 2
after_mini=$(message_count "$url_a" "$pass_a" "$project_a" "$session_a" MINI_TUI_ROUTE_PROBE)
[ "$after_mini" -eq "$before_mini" ] || fail '--mini unexpectedly accepted /tui/*; update the topology evidence'
capture_tui mini-a | grep -F MINI_TUI_ROUTE_PROBE >/dev/null && fail '--mini unexpectedly rendered /tui/*; update the topology evidence'

api_post "$url_b" "$pass_b" "/background-process/$background_id/stop" "$project_b" >/dev/null
background_id=

result="$scratch/result.json"
jq -n \
  --arg version "$actual_version" \
  --arg sessionA "$session_a" \
  --arg sessionB "$session_b" \
  --argjson routeAOwn "$(message_count "$url_a" "$pass_a" "$project_a" "$session_a" ROUTE_A_MODEL)" \
  --argjson routeAForeign "$(message_count "$url_a" "$pass_a" "$project_a" "$session_a" ROUTE_B_MODEL)" \
  --argjson routeBOwn "$(message_count "$url_b" "$pass_b" "$project_b" "$session_b" ROUTE_B_MODEL)" \
  --argjson routeBForeign "$(message_count "$url_b" "$pass_b" "$project_b" "$session_b" ROUTE_A_MODEL)" \
  --arg userOrder "$user_order" \
  --argjson miniBefore "$before_mini" \
  --argjson miniAfter "$after_mini" \
  '{
    type:"six_product.phase0.kilo",
    result:"GREEN",
    kilo_version:$version,
    topology:"one authenticated kilo serve plus one full kilo attach TUI per peer",
    sessions:{a:$sessionA,b:$sessionB},
    routing:{a_own:$routeAOwn,a_foreign:$routeAForeign,b_own:$routeBOwn,b_foreign:$routeBForeign},
    busy_queue_order:($userOrder|fromjson),
    assertions:{
      two_pair_exact_routing:true,
      zero_cross_delivery:true,
      full_attach_idle_wake:true,
      full_attach_busy_queue:true,
      background_process_exact_session:true,
      mcp_connected_both_pairs:true,
      rename_visible_and_resume_same_native_session:true,
      completion_busy_idle_events:true
    },
    negative:{mini_tui_route_supported:false,before:$miniBefore,after:$miniAfter},
    credentials_persisted:false
  }' >"$result"
jq -e '.result == "GREEN" and .routing.a_foreign == 0 and .routing.b_foreign == 0 and ([.assertions[]] | all)' "$result" >/dev/null
printf 'S1 GREEN\n'
cat "$result"
