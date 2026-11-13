#!/bin/bash
# 9front-mcp in a 9front guest (../9front-gccwt's scripts/guest/boot-cli.sh, as
# ../9front-go's tests do), reached from the host over HTTP: a second NIC whose
# QEMU user network forwards 127.0.0.1:$PORT to the guest's port 8090.
# Checks the protocol and each tool with curl; CLAUDE=1 then also lets Claude
# Code work in the guest through it (scripts/claude-9front).
# usage: [objtype=amd64|386] [PORT=18091] [CLAUDE=1] scripts/guest-test.sh
set -e
TOP=$(cd "$(dirname "$0")/.." && pwd)
. "$TOP/scripts/nfront-paths.sh"	# TREE, TREE_GCC, GCC_DIR, ... (the distribution's paths.env)
PLAN9=$(cd "${PLAN9:-$TREE_GCC}" && pwd)
GO=${GO:-$GO_DIR/go/bin/go}
A=${objtype:-amd64}
PORT=${PORT:-18091}
TOKEN=guesttest$$
CLI_DIR=${CLI_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/nfront/cli}
mkdir -p "$TOP/build"
T=$(mktemp -d -p "$TOP/build")
mkdir -p "$T/extra/tests"
GOOS=plan9 GOARCH=$A CGO_ENABLED=0 GOCACHE=$TOP/build/gocache "$GO" build -o "$T/extra/tests/9front-mcp" "$TOP"
cp "$TOP/deny.example" "$T/extra/tests/deny"

cd "$PLAN9"
# its own process group, so that it goes as a whole (QEMU, ufs)
objtype=$A HOLD=1 SMP=2 UFS_EXTRA=$T/extra \
	QEMU_EXTRA="-netdev user,id=net1,net=10.0.3.0/24,hostfwd=tcp:127.0.0.1:$PORT-10.0.3.15:8090 -device e1000,netdev=net1" \
	setsid bash scripts/guest/boot-cli.sh \
	"bind -a '#l1' /net; ip/ipconfig ether /net/ether1 10.0.3.15 255.255.255.0" \
	"cat /net/ipselftab | grep 10.0.3" \
	"mkdir -p /tmp/mcp; /tests/9front-mcp -http tcp!*!8090 -token $TOKEN -dir /tmp/mcp -deny /tests/deny -v >[2]/tmp/mcp.log &" \
	"sleep 2; cat /tmp/mcp.log; ls /dev | grep '^sd'" > "$T/log" 2>&1 &
guest=$!
# under set -e the guest's status (143, from the TERM) would end the trap and become the script's
trap 'st=$?; kill -TERM -- -$guest 2>/dev/null; wait $guest 2>/dev/null || true; rm -rf "$T"; exit $st' EXIT

URL=http://127.0.0.1:$PORT/mcp
up=0
for _ in $(seq 150); do
	if curl -s -o /dev/null -m 2 -H "Authorization: Bearer $TOKEN" "$URL"; then up=1; break; fi
	kill -0 $guest 2>/dev/null || break
	sleep 2
done
tr -d '\r' < "$T/log" | sed -n '/^init: starting/,$p'
[ $up = 1 ] || { echo "guest-test: the server did not come up" >&2; tail -30 "$T/log"; exit 1; }

fail=0
sid=
rpc() {	# rpc JSON: prints the answer's body
	curl -s -m 120 -D "$T/hdr" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
		-H 'Accept: application/json, text/event-stream' ${sid:+-H "Mcp-Session-Id: $sid"} -d "$1" "$URL"
}
check() {	# check NAME JSON EXPECTED-SUBSTRING
	local out
	out=$(rpc "$2")
	if grep -qF -- "$3" <<<"$out"; then echo "PASS $1"; else echo "FAIL $1: $out"; fail=1; fi
}
call() {	# call TOOL ARGS-JSON
	printf '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"%s","arguments":%s}}' "$1" "$2"
}
check initialize '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}' '"serverInfo"'
sid=$(tr -d '\r' < "$T/hdr" | sed -n 's/^[Mm]cp-[Ss]ession-[Ii]d: //p')
check tools/list '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' '"name":"rc"'
check rc "$(call rc '{"command":"echo $objtype; cat /dev/sysname"}')" "$A"
check rc-status "$(call rc '{"command":"rm /nonexistent"}')" "status: rm "
check rc-timeout "$(call rc '{"command":"sleep 30","timeout":1}')" 'timed out'
check write "$(call write '{"path":"d/a.c","content":"#include <u.h>\n#include <libc.h>\n\nvoid\nmain(void)\n{\n\tprint(\"hello\\n\");\n\texits(nil);\n}\n"}')" 'wrote /tmp/mcp/d/a.c'
check read "$(call read '{"path":"d/a.c"}')" '     7\t\tprint('
check edit "$(call edit '{"path":"d/a.c","old_string":"hello","new_string":"hi 9front"}')" '1 replacement'
check glob "$(call glob '{"pattern":"**/*.c"}')" '/tmp/mcp/d/a.c'
check grep "$(call grep '{"pattern":"hi 9front","output_mode":"content"}')" '/tmp/mcp/d/a.c:7:'
# the guest's root (gen-ufs-root.sh) has the binaries and rc scripts, no headers or libraries
check grep-root "$(call grep '{"pattern":"^fn ","path":"/rc/bin","output_mode":"count"}')" '/rc/bin/'
check glob-sys "$(call glob '{"pattern":"/'$A'/bin/{rc,ls}"}')" "/$A/bin/rc"
check read-dev "$(call read '{"path":"/dev/sysname"}')" '     1\t'
check rc-list "$(call rc '{"command":"x=(a b c); echo $#x $x(2)"}')" '3 b'
check rc-pipe "$(call rc '{"command":"ls /'$A'/bin/rc /'$A'/bin/ls | grep -c bin"}')" '"text":"2\n"'
check rc-no-fn "$(call rc '{"command":"echo clean"}')" '"text":"clean\n"'
# the disks: gone from the commands' name space, and #S cannot come back
check disk-gone "$(call rc '{"command":"ls /dev | grep -c sd"}')" '"text":"0\n'
check disk-rebind "$(call rc '{"command":"d='"\\u0027#\\u0027"'^S; bind -a $d /dev >[2=1]; ls /dev | grep -c sd"}')" 'disallowed\n0\n'
check disk-write "$(call write '{"path":"/dev/sdctl","content":"x"}')" 'is a disk device'
check deny-rc "$(call rc '{"command":"disk/prep -w /dev/sdC0/plan9"}')" 'deny list (rule'
check deny-path "$(call write '{"path":"/adm/users","content":"x"}')" 'deny list (rule'
check deny-ok "$(call rc '{"command":"echo mkfs.c /tmp/mbr"}')" '"text":"mkfs.c /tmp/mbr\n"'

if [ -n "$CLAUDE" ]; then
	echo "--- Claude Code in the guest"
	echo "Find out which Plan 9 system this is (cat /dev/sysname, the kernel in /dev/osversion), then in /tmp/mcp write a C program cnt.c for Plan 9 (u.h, libc.h) that prints the number of arguments, compile and run it with three arguments, and say what it printed." |
		MCPTOKEN=$TOKEN timeout 600 "$TOP/scripts/claude-9front" -url "$URL" -- -p --model "${MODEL:-haiku}" --max-turns 20 | tee "$T/claude.out"
	grep -q '3' "$T/claude.out" && echo "PASS claude" || { echo "FAIL claude"; fail=1; }
fi
echo "--- server log"
rpc "$(call rc '{"command":"cat /tmp/mcp.log"}')" | python3 -c 'import sys,json; print(json.load(sys.stdin)["result"]["content"][0]["text"])' | tail -20
[ $fail = 0 ] && echo "$A: all passed"
exit $fail
