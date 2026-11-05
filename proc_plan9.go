package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const defaultShell = "/bin/rc"

// newGroup gives the command a note group of its own, so that a timeout
// kills everything it started, and a copy of the name space, so that its
// binds and mounts stay its own.
//
// The environment goes without rc's functions (fn#name): they are the
// server's shell's (sigexit & co.), not the command's. (Go before
// ../9front-go's patch 0006 also wrote them back to /env with a NUL at the
// end, which rc cannot parse: "/env/fn#sigexit:1: syntax error".)
func newGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Rfork: syscall.RFNOTEG | syscall.RFNAMEG}
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "fn#") {
			env = append(env, e)
		}
	}
	cmd.Env = env
}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := os.WriteFile("/proc/"+strconv.Itoa(cmd.Process.Pid)+"/notepg", []byte("kill"), 0)
	if err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

// exitStatus is the process's exit string, as rc's $status shows it. Go
// before ../9front-go's patch 0006 leaves the wait message's quoting on it
// (in single quotes, a quote inside doubled); a message itself starts with
// the program's name, never with a quote.
func exitStatus(ee *exec.ExitError) string {
	if w, ok := ee.Sys().(*syscall.Waitmsg); ok && w.Msg != "" {
		return unquote(w.Msg)
	}
	return ee.String()
}

// disk drivers: sd(3), floppy, ATA over Ethernet
const diskDevs = "Sfæ"

// diskGuard returns the rc lines that take the disks away from a command's
// name space before it runs: unmount every bind of a disk driver the server's
// name space has (/dev/sdE0 & co.), then chdev(1)'s mask in #c/drivers, so that
// no '#S' can be bound again. The mask holds for the name group and all its
// children, and only the kernel could lift it. File systems already mounted
// from a disk stay as they are.
//
// paths are what the server's own file tools must not write: the disk
// drivers' files where they are bound, and the drivers themselves.
func diskGuard() (guard string, paths []string, err error) {
	ns, err := os.ReadFile("/proc/" + strconv.Itoa(os.Getpid()) + "/ns")
	if err != nil {
		return "", nil, err
	}
	for _, r := range diskDevs {
		paths = append(paths, "#"+string(r))
	}
	var b strings.Builder
	for _, line := range strings.Split(string(ns), "\n") {
		f := tokenize(line)
		// bind [-abc] '#S' /dev; mount lines have a server, not a driver
		if len(f) < 3 || f[0] != "bind" {
			continue
		}
		src, dst := f[len(f)-2], f[len(f)-1]
		if strings.HasPrefix(src, "#") && strings.ContainsAny(firstRune(src[1:]), diskDevs) {
			b.WriteString("unmount " + rcQuote(src) + " " + rcQuote(dst) + " >[2]/dev/null\n")
			ents, _ := os.ReadDir(src)
			for _, e := range ents {
				paths = append(paths, filepath.Join(dst, e.Name()))
			}
		}
	}
	b.WriteString("echo 'chdev &~ " + diskDevs + "' >'#c/drivers' || exit 'no disk guard'\n")
	return b.String(), paths, nil
}
