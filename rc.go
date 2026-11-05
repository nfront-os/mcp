package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const maxRcOutput = 30000

func runRc(ctx context.Context, cfg *Config, ss *Session, args json.RawMessage) (string, error) {
	var a struct {
		Command string `json:"command"`
		Dir     string `json:"dir"`
		Timeout int    `json:"timeout"`
	}
	if err := unmarshal(args, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", errors.New("command is empty")
	}
	if r := denied(cfg.Deny, a.Command, false); r != nil {
		return "", r
	}
	dir := cfg.Dir
	if a.Dir != "" {
		var err error
		if dir, err = resolve(cfg, a.Dir); err != nil {
			return "", err
		}
	}
	if fi, err := os.Stat(dir); err != nil {
		return "", err
	} else if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	timeout := 120 * time.Second
	if a.Timeout > 0 {
		timeout = time.Duration(min(a.Timeout, 600)) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if cfg.Guard != "" {
		// the guard runs first in the command's own name group, then the
		// command in an rc of its own: a syntax error in it cannot keep
		// the guard from running
		cmd = exec.CommandContext(ctx, cfg.Shell, "-c", cfg.Guard+"exec "+rcQuote(cfg.Shell)+" -c $mcpcmd")
	} else {
		cmd = exec.CommandContext(ctx, cfg.Shell, "-c", a.Command)
	}
	cmd.Dir = dir
	var out capBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	newGroup(cmd)
	if cfg.Guard != "" {
		if cmd.Env == nil {
			cmd.Env = os.Environ()
		}
		cmd.Env = append(cmd.Env, "mcpcmd="+a.Command)
	}
	cmd.Cancel = func() error { return killGroup(cmd) }
	// a program left in the background keeps the output pipe open: Wait
	// gives up on it after this
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	text := capOutput(out.String(), maxRcOutput)
	if ctx.Err() == context.DeadlineExceeded {
		return text, fmt.Errorf("timed out after %v: note group killed", timeout)
	}
	if ctx.Err() != nil {
		return text, errors.New("cancelled: note group killed")
	}
	status := ""
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			status = exitStatus(ee)
		} else if !errors.Is(err, exec.ErrWaitDelay) {
			return text, err
		}
	}
	if text == "" {
		text = "(no output)\n"
	} else if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if status != "" {
		text += "status: " + status + "\n"
	}
	return text, nil
}

// capBuffer keeps the first and the last 256 KB of what is written to it.
type capBuffer struct {
	mu    sync.Mutex
	head  []byte
	tail  []byte
	total int
}

const capHalf = 256 << 10

func (b *capBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n0 := len(p)
	b.total += n0
	if n := capHalf - len(b.head); n > 0 {
		k := min(n, len(p))
		b.head = append(b.head, p[:k]...)
		p = p[k:]
	}
	if len(p) > 0 {
		b.tail = append(b.tail, p...)
		if len(b.tail) > capHalf {
			b.tail = append(b.tail[:0:0], b.tail[len(b.tail)-capHalf:]...)
		}
	}
	return n0, nil
}

func (b *capBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	cut := b.total - len(b.head) - len(b.tail)
	if cut > 0 {
		return fmt.Sprintf("%s\n[... %d bytes cut ...]\n%s", b.head, cut, b.tail)
	}
	return string(b.head) + string(b.tail)
}
