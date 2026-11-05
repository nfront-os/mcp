# 9front-mcp

An MCP server for 9front (Plan 9) in Go, with nothing outside Go's standard library.
It lets an agent running on another machine work on a 9front system: Claude Code
runs on Linux with its built-in tools switched off, and every file operation and
command goes through this server and runs natively on 9front.

Tools: `read`, `write`, `edit` (files), `glob`, `grep` (search), `rc` (a command
line in rc(1), with a timeout that kills its note group). The server describes
the Plan 9 environment to the agent (`instructions` at initialize): rc rather
than sh, mk, walk, and so on.

Transport: MCP's Streamable HTTP (POST `/mcp`, JSON answers, `Mcp-Session-Id`,
DELETE ends a session; no server-sent streams), or `-stdio`.

## Build

    GOOS=plan9 GOARCH=amd64 ../9front-go/go/bin/go build -o 9front-mcp .    # 386, arm64 too

Any Go that has plan9/amd64 builds it as well; `../9front-go` also does arm64.

## On 9front

    9front-mcp -http 'tcp!*!8090' -tokenfile /usr/glenda/lib/mcptoken -deny /sys/lib/mcp/deny -dir /usr/glenda/src

Without `-token`, `-tokenfile` or `$MCPTOKEN`, it makes up a token and prints it.
`-tlscert`/`-tlskey` serve HTTPS. `-v` logs each call.

## On the Linux side

    MCPTOKEN=... scripts/claude-9front -url http://9front-host:8090/mcp

This runs `claude` with `--tools ""` (no built-in tools), `--strict-mcp-config`
(only this server), the server's tools allowed without asking (`-ask` asks each
time), and a system prompt saying that the work is on the 9front system.
Arguments for `claude` go after `--`; `CLAUDE_TOOLS=WebSearch,WebFetch` keeps
built-in tools that do not touch the local machine.

By hand:

    claude mcp add --transport http 9front http://9front-host:8090/mcp --header "Authorization: Bearer $MCPTOKEN"
    claude --tools "" --allowedTools mcp__9front

## Safety

The `rc` tool runs any command, as the user who started the server (on a
terminal that is usually the host owner). What stands between the agent and
the machine:

- **Token.** HTTP requests need `Authorization: Bearer <token>`. Requests with
  an `Origin` header (browsers) are refused unless `-origin` allows them.
  Without TLS the token travels in clear text: use it on a network you trust.
- **Disks (on by default; `-disks` turns it off).** Every rc command first
  unmounts the disk drivers' binds (`#S`, floppy, AoE: `/dev/sdE0` & co.) from
  its own name space, then masks the drivers (`chdev &~ Sfæ` in
  `#c/drivers`, see chdev(1)). The kernel then refuses `#S` to that name group
  and all its children, and nothing in user space can lift the mask. `write`
  and `edit` refuse the disk devices too; `read` may read them. File systems
  already mounted from a disk stay reachable as they are.
- **Deny list (`-deny file`).** Commands and write/edit paths matching its rules
  are refused (`deny.example`: partitioning, formatting, reaming file servers,
  writing to `/dev/sd*`, fshalt/reboot, `rm -r /`, `/adm/users`). It is a guard
  against mistakes, not a sandbox: rc can spell any command so that no pattern
  sees it (`d='#'^S`, eval, a script). The agent is told not to work around a
  refusal.

What it does not stop: a host-owner process can still reach other host-owner
processes (their `/proc/n/mem`, `ctl`), and through one that still has the
disks, the disks. For a hard boundary run the server as a user that owns
nothing it must not touch.

## Tests

    go test ./...              # on Linux: the protocol, the tools (rc as /bin/sh)
    scripts/guest-test.sh      # in a 9front guest (../9front-gccwt's boot-cli.sh): every tool,
                               # the disk guard and the deny list over HTTP from the host
    CLAUDE=1 scripts/guest-test.sh   # and Claude Code working in the guest (haiku)

## Notes on Go for Plan 9

Found while writing this. The first two are upstream Go's code, fixed in
`../9front-go` (patch 0006); the server copes with an unfixed Go as well:

- `syscall/exec_plan9.go` wrote every environment value to the child's `/env`
  with a NUL at the end. For a list that is the terminator rc expects, but an
  rc function (`fn#name`) then failed to parse in the child (`/env/fn#sigexit:1:
  syntax error`). The server leaves the functions out anyway: they are its
  shell's, not the commands'.
- `Waitmsg.Msg` kept the wait message's quoting (`'rm 269: ... ''/x'''`);
  the server unquotes it.
- Not fixed: after `syscall.Exec` the Go runtime's other procs stay (the
  server does not use it).
- The Go runtime is several procs sharing memory, so one proc's
  `rfork(RFNAMEG)` would split the runtime's name space. Restrictions are
  therefore applied in each command's own name group (an rc prefix), not in the
  server.
