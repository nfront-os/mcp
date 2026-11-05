package main

// The file tools: read, write, edit. glob and grep are in search.go, rc in
// rc.go.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
	Run         func(ctx context.Context, cfg *Config, ss *Session, args json.RawMessage) (string, error)
}

func toolList() []map[string]any {
	var l []map[string]any
	for _, t := range tools {
		l = append(l, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.Schema})
	}
	return l
}

func findTool(name string) *Tool {
	for _, t := range tools {
		if t.Name == name {
			return t
		}
	}
	return nil
}

func object(required []string, props map[string]any) map[string]any {
	o := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

var tools []*Tool

func init() {
	tools = []*Tool{
		{
			Name: "read",
			Description: "Read a file on the 9front system. Lines come numbered (cat -n style: number, tab, text), " +
				"from offset (first line 1) for limit lines (2000 by default); lines longer than 2000 characters " +
				"are cut. A directory gives its entries, directories with a trailing /. Read a file before " +
				"overwriting it with write. Synthetic files that block (/dev/cons, /net/*/listen) are for rc.",
			Schema: object([]string{"path"}, map[string]any{
				"path":   str("Path of the file, absolute or relative to the server's directory."),
				"offset": num("Line to start at, 1 is the first."),
				"limit":  num("Number of lines to read."),
			}),
			Run: runRead,
		},
		{
			Name: "write",
			Description: "Write a file on the 9front system, replacing what it held; missing directories are made. " +
				"An existing file must have been read first (read), and not changed since. Prefer edit for " +
				"changes to an existing file.",
			Schema: object([]string{"path", "content"}, map[string]any{
				"path":    str("Path of the file, absolute or relative to the server's directory."),
				"content": str("The whole new content."),
			}),
			Run: runWrite,
		},
		{
			Name: "edit",
			Description: "Replace text in a file on the 9front system. old_string must occur exactly once (give more " +
				"context to make it unique) unless replace_all is set. Copy old_string exactly as read shows the " +
				"text, without the line-number prefix; keep tabs as tabs.",
			Schema: object([]string{"path", "old_string", "new_string"}, map[string]any{
				"path":        str("Path of the file, absolute or relative to the server's directory."),
				"old_string":  str("The text to replace."),
				"new_string":  str("The text to put in its place (must differ from old_string)."),
				"replace_all": boolean("Replace every occurrence."),
			}),
			Run: runEdit,
		},
		{
			Name: "glob",
			Description: "Find files on the 9front system by name pattern: * ? [...] within a path element, ** " +
				"for any number of directories, {a,b} for alternatives, e.g. **/*.c or sys/src/cmd/{rc,sam}/*.h. " +
				"Newest first, at most 1000. .git and synthetic file systems (/proc, /dev, /net, /srv, /env, " +
				"/fd, /mnt, /n) are skipped unless path is in them.",
			Schema: object([]string{"pattern"}, map[string]any{
				"pattern": str("The pattern, relative to path (or absolute)."),
				"path":    str("Directory to search, the server's directory by default."),
			}),
			Run: runGlob,
		},
		{
			Name: "grep",
			Description: "Search file contents on the 9front system with a regular expression (Go's RE2 syntax: " +
				"\\d, \\b, (?i), no backreferences), recursively under path. Binary files and .git are skipped, " +
				"as glob skips synthetic file systems. output_mode: files_with_matches (default: the files, " +
				"newest first), content (path:line:text, with context lines path-line-text), count (path:n).",
			Schema: object([]string{"pattern"}, map[string]any{
				"pattern":     str("Regular expression."),
				"path":        str("File or directory to search, the server's directory by default."),
				"glob":        str("Only files whose name (or path below path, if it has a /) matches this glob, e.g. *.c or *.{c,h}."),
				"ignore_case": boolean("Case-insensitive search."),
				"output_mode": map[string]any{"type": "string", "enum": []string{"files_with_matches", "content", "count"},
					"description": "What to show; files_with_matches by default."},
				"context":    num("Lines of context before and after each match (content mode)."),
				"head_limit": num("At most this many lines of output (500 by default)."),
			}),
			Run: runGrep,
		},
		{
			Name: "rc",
			Description: "Run a command line in rc(1) on the 9front system and return its output (stdout and stderr " +
				"together) and exit status. rc, not sh: if(test -f x) echo yes; for(f in *.c) echo $f; " +
				"'single quotes' only; >[2=1] for 2>&1; `{cmd} for $(cmd); $status is '' on success. Each call " +
				"starts afresh (no cd, variables or binds carry over) in dir. stdin is /dev/null. Output past " +
				"30000 characters is cut in the middle. The server may refuse commands (its deny list) and keep the " +
				"disks from it: do not try to get around that, ask the user.",
			Schema: object([]string{"command"}, map[string]any{
				"command": str("The rc command line(s)."),
				"dir":     str("Directory to run in, the server's directory by default."),
				"timeout": num("Seconds before the command's note group is killed (120 by default, at most 600)."),
			}),
			Run: runRc,
		},
	}
}

func resolve(cfg *Config, p string) (string, error) {
	if p == "" {
		return "", errors.New("path is empty")
	}
	if !filepath.IsAbs(p) && !strings.HasPrefix(p, "#") {
		p = filepath.Join(cfg.Dir, p)
	}
	return filepath.Clean(p), nil
}

func unmarshal(args json.RawMessage, v any) error {
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("arguments: %v", err)
	}
	return nil
}

// fileStamp is what read saw of a file, so that write and edit can tell
// whether it changed since.
type fileStamp struct {
	mtime time.Time
	size  int64
}

func stamp(fi fs.FileInfo) fileStamp {
	return fileStamp{fi.ModTime(), fi.Size()}
}

func (ss *Session) remember(path string) {
	if fi, err := os.Stat(path); err == nil {
		ss.mu.Lock()
		ss.reads[path] = stamp(fi)
		ss.mu.Unlock()
	}
}

func (ss *Session) seen(path string) (fileStamp, bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	st, ok := ss.reads[path]
	return st, ok
}

const maxLine = 2000

func runRead(ctx context.Context, cfg *Config, ss *Session, args json.RawMessage) (string, error) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := unmarshal(args, &a); err != nil {
		return "", err
	}
	p, err := resolve(cfg, a.Path)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return readDir(p)
	}
	if a.Offset < 1 {
		a.Offset = 1
	}
	if a.Limit <= 0 {
		a.Limit = 2000
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	head, _ := r.Peek(8000)
	if bytes.IndexByte(head, 0) >= 0 {
		return fmt.Sprintf("%s: binary file, %d bytes (use rc, e.g. xd -c %s)", p, fi.Size(), p), nil
	}
	var b strings.Builder
	n, shown := 0, 0
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		line, err := r.ReadString('\n')
		if line == "" && err != nil {
			if err != io.EOF {
				return b.String(), err
			}
			break
		}
		n++
		if n >= a.Offset && shown < a.Limit {
			line = strings.TrimSuffix(line, "\n")
			if utf8.RuneCountInString(line) > maxLine {
				line = string([]rune(line)[:maxLine]) + " [line cut]"
			}
			fmt.Fprintf(&b, "%6d\t%s\n", n, line)
			shown++
		}
		if err != nil {
			break
		}
	}
	ss.remember(p)
	switch {
	case n == 0:
		return p + ": empty file", nil
	case shown == 0:
		return fmt.Sprintf("%s has %d lines; nothing from line %d on", p, n, a.Offset), nil
	case a.Offset+shown-1 < n:
		fmt.Fprintf(&b, "[lines %d-%d of %d; give offset to read on]\n", a.Offset, a.Offset+shown-1, n)
	}
	return b.String(), nil
}

func readDir(p string) (string, error) {
	ents, err := os.ReadDir(p)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() {
			n += "/"
		}
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return p + ": empty directory", nil
	}
	return strings.Join(names, "\n") + "\n", nil
}

func runWrite(ctx context.Context, cfg *Config, ss *Session, args json.RawMessage) (string, error) {
	var a struct {
		Path    string  `json:"path"`
		Content *string `json:"content"`
	}
	if err := unmarshal(args, &a); err != nil {
		return "", err
	}
	if a.Content == nil {
		return "", errors.New("content is missing")
	}
	p, err := resolve(cfg, a.Path)
	if err != nil {
		return "", err
	}
	if err := writable(cfg, p); err != nil {
		return "", err
	}
	fi, err := os.Stat(p)
	switch {
	case err == nil && fi.IsDir():
		return "", fmt.Errorf("%s is a directory", p)
	case err == nil:
		if err := unchanged(ss, p, fi, true); err != nil {
			return "", err
		}
		if err := overwrite(p, []byte(*a.Content)); err != nil {
			return "", err
		}
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(p), 0777); err != nil {
			return "", err
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0666)
		if err != nil {
			return "", err
		}
		_, err = f.Write([]byte(*a.Content))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", err
		}
	default:
		return "", err
	}
	ss.remember(p)
	return fmt.Sprintf("wrote %s (%d bytes)", p, len(*a.Content)), nil
}

// unchanged checks that the file is as the session last read it; mustRead:
// that it was read at all.
func unchanged(ss *Session, p string, fi fs.FileInfo, mustRead bool) error {
	st, ok := ss.seen(p)
	if !ok {
		if mustRead {
			return fmt.Errorf("%s exists: read it before overwriting it", p)
		}
		return nil
	}
	if st != stamp(fi) {
		return fmt.Errorf("%s changed since it was read: read it again", p)
	}
	return nil
}

// overwrite truncates and rewrites the file in place: its mode, owner and
// group stay as they are (Plan 9 has no atomic rename over a file).
func overwrite(p string, data []byte) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func runEdit(ctx context.Context, cfg *Config, ss *Session, args json.RawMessage) (string, error) {
	var a struct {
		Path       string `json:"path"`
		Old        string `json:"old_string"`
		New        string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := unmarshal(args, &a); err != nil {
		return "", err
	}
	if a.Old == "" {
		return "", errors.New("old_string is empty (use write to make a file)")
	}
	if a.Old == a.New {
		return "", errors.New("old_string and new_string are the same")
	}
	p, err := resolve(cfg, a.Path)
	if err != nil {
		return "", err
	}
	if err := writable(cfg, p); err != nil {
		return "", err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return "", fmt.Errorf("%s is a directory", p)
	}
	if err := unchanged(ss, p, fi, false); err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	text := string(data)
	n := strings.Count(text, a.Old)
	switch {
	case n == 0:
		return "", fmt.Errorf("old_string not found in %s", p)
	case n > 1 && !a.ReplaceAll:
		return "", fmt.Errorf("old_string occurs %d times in %s: give more context or set replace_all", n, p)
	}
	at := strings.Index(text, a.Old)
	if a.ReplaceAll {
		text = strings.ReplaceAll(text, a.Old, a.New)
	} else {
		text = text[:at] + a.New + text[at+len(a.Old):]
	}
	if err := overwrite(p, []byte(text)); err != nil {
		return "", err
	}
	ss.remember(p)
	if !a.ReplaceAll {
		n = 1
	}
	return fmt.Sprintf("edited %s: %d replacement(s)\n%s", p, n, snippet(text, at, len(a.New))), nil
}

// snippet shows the lines around text[at:at+n] numbered, three before and after.
func snippet(text string, at, n int) string {
	first := strings.Count(text[:at], "\n") + 1
	last := first + strings.Count(text[at:at+n], "\n")
	lines := strings.Split(text, "\n")
	lo, hi := max(first-3, 1), min(last+3, len(lines))
	var b strings.Builder
	for i := lo; i <= hi; i++ {
		l := lines[i-1]
		if utf8.RuneCountInString(l) > maxLine {
			l = string([]rune(l)[:maxLine]) + " [line cut]"
		}
		fmt.Fprintf(&b, "%6d\t%s\n", i, l)
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
