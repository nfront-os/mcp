package main

// glob and grep: a walk of the tree under a directory that leaves out .git
// and the kernel's synthetic file systems, glob patterns with ** and {a,b}.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// skipped when a walk reaches them from above: reading them can block, or
// they are other machines' trees (/n, /mnt)
var synthetic = map[string]bool{
	"/proc": true, "/dev": true, "/net": true, "/net.alt": true, "/srv": true,
	"/env": true, "/fd": true, "/mnt": true, "/n": true,
}

type found struct {
	path  string
	rel   string
	mtime time.Time
}

// walk calls fn for every file under root (root itself, if it is a file).
func walk(ctx context.Context, root string, fn func(f found) error) error {
	fi, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fn(found{root, filepath.Base(root), fi.ModTime()})
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if p == root {
				return err
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != root && (d.Name() == ".git" || synthetic[p]) {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		return fn(found{p, filepath.ToSlash(rel), info.ModTime()})
	})
}

// expandBraces turns a{b,c}d into abd and acd (nested braces too).
func expandBraces(p string) []string {
	depth, start := 0, -1
	for i, c := range p {
		switch c {
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 {
				var out []string
				for _, alt := range splitTop(p[start+1 : i]) {
					out = append(out, expandBraces(p[:start]+alt+p[i+1:])...)
				}
				return out
			}
		}
	}
	return []string{p}
}

// splitTop splits at the commas not inside nested braces.
func splitTop(s string) []string {
	var parts []string
	depth, last := 0, 0
	for i, c := range s {
		switch c {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[last:i])
				last = i + 1
			}
		}
	}
	return append(parts, s[last:])
}

type globPattern [][]string // alternatives, each split at /

func compileGlob(p string) (globPattern, error) {
	var g globPattern
	for _, alt := range expandBraces(p) {
		segs := strings.Split(strings.Trim(alt, "/"), "/")
		for _, s := range segs {
			if s != "**" {
				if _, err := path.Match(s, ""); err != nil {
					return nil, fmt.Errorf("bad pattern %q", p)
				}
			}
		}
		g = append(g, segs)
	}
	return g, nil
}

func (g globPattern) match(rel string) bool {
	names := strings.Split(rel, "/")
	for _, segs := range g {
		if matchSegs(segs, names) {
			return true
		}
	}
	return false
}

func matchSegs(segs, names []string) bool {
	for len(segs) > 0 {
		if segs[0] == "**" {
			for len(segs) > 0 && segs[0] == "**" {
				segs = segs[1:]
			}
			if len(segs) == 0 {
				return true
			}
			for i := range names {
				if matchSegs(segs, names[i:]) {
					return true
				}
			}
			return false
		}
		if len(names) == 0 {
			return false
		}
		if ok, _ := path.Match(segs[0], names[0]); !ok {
			return false
		}
		segs, names = segs[1:], names[1:]
	}
	return len(names) == 0
}

// staticPrefix splits an absolute pattern into the directory before its
// first pattern character and the rest.
func staticPrefix(p string) (dir, rest string) {
	segs := strings.Split(p, "/")
	i := 0
	for ; i < len(segs)-1; i++ {
		if strings.ContainsAny(segs[i], "*?[{") {
			break
		}
	}
	dir = strings.Join(segs[:i], "/")
	if dir == "" {
		dir = "/"
	}
	return dir, strings.Join(segs[i:], "/")
}

func runGlob(ctx context.Context, cfg *Config, ss *Session, args json.RawMessage) (string, error) {
	var a struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := unmarshal(args, &a); err != nil {
		return "", err
	}
	if a.Pattern == "" {
		return "", errors.New("pattern is empty")
	}
	root := cfg.Dir
	if a.Path != "" {
		var err error
		if root, err = resolve(cfg, a.Path); err != nil {
			return "", err
		}
	}
	pat := a.Pattern
	if strings.HasPrefix(pat, "/") {
		root, pat = staticPrefix(path.Clean(pat))
	}
	g, err := compileGlob(pat)
	if err != nil {
		return "", err
	}
	var hits []found
	err = walk(ctx, root, func(f found) error {
		if g.match(f.rel) {
			hits = append(hits, f)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "no files match", nil
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].mtime.After(hits[j].mtime) })
	var b strings.Builder
	for i, h := range hits {
		if i == 1000 {
			fmt.Fprintf(&b, "[%d more; narrow the pattern]\n", len(hits)-1000)
			break
		}
		b.WriteString(h.path)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

const maxGrepFile = 64 << 20
const maxGrepLine = 500

func runGrep(ctx context.Context, cfg *Config, ss *Session, args json.RawMessage) (string, error) {
	var a struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Glob       string `json:"glob"`
		IgnoreCase bool   `json:"ignore_case"`
		Mode       string `json:"output_mode"`
		Context    int    `json:"context"`
		HeadLimit  int    `json:"head_limit"`
	}
	if err := unmarshal(args, &a); err != nil {
		return "", err
	}
	expr := a.Pattern
	if a.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", err
	}
	root := cfg.Dir
	if a.Path != "" {
		if root, err = resolve(cfg, a.Path); err != nil {
			return "", err
		}
	}
	var g globPattern
	if a.Glob != "" {
		if g, err = compileGlob(a.Glob); err != nil {
			return "", err
		}
	}
	byPath := strings.Contains(a.Glob, "/")
	if a.Mode == "" {
		a.Mode = "files_with_matches"
	}
	if a.Mode != "files_with_matches" && a.Mode != "content" && a.Mode != "count" {
		return "", fmt.Errorf("unknown output_mode %q", a.Mode)
	}
	if a.HeadLimit <= 0 {
		a.HeadLimit = 500
	}
	a.Context = max(0, min(a.Context, 20))

	type fileHits struct {
		found
		n     int
		lines []string
	}
	var files []fileHits
	out := 0 // lines of output so far (content mode)
	err = walk(ctx, root, func(f found) error {
		if g != nil {
			name := path.Base(f.rel)
			if byPath {
				name = f.rel
			}
			if !g.match(name) {
				return nil
			}
		}
		if a.Mode == "content" && out >= a.HeadLimit {
			return fs.SkipAll
		}
		fh := fileHits{found: f}
		fh.n, fh.lines = grepFile(f.path, re, a.Mode == "content", a.Context)
		if fh.n > 0 {
			files = append(files, fh)
			out += len(fh.lines)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "no matches", nil
	}
	var lines []string
	switch a.Mode {
	case "files_with_matches":
		sort.SliceStable(files, func(i, j int) bool { return files[i].mtime.After(files[j].mtime) })
		for _, f := range files {
			lines = append(lines, f.path)
		}
	case "count":
		for _, f := range files {
			lines = append(lines, fmt.Sprintf("%s:%d", f.path, f.n))
		}
	case "content":
		for _, f := range files {
			lines = append(lines, f.lines...)
		}
	}
	more := 0
	if len(lines) > a.HeadLimit {
		more = len(lines) - a.HeadLimit
		lines = lines[:a.HeadLimit]
	}
	s := strings.Join(lines, "\n") + "\n"
	if more > 0 || out > a.HeadLimit {
		s += "[output cut at head_limit; narrow the search]\n"
	}
	return capOutput(s, 50000), nil
}

// grepFile counts the lines of p that match; with content, it returns them
// as path:n:text, context lines as path-n-text, -- between groups.
func grepFile(p string, re *regexp.Regexp, content bool, ctxLines int) (int, []string) {
	f, err := os.Open(p)
	if err != nil {
		return 0, nil
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || fi.Size() > maxGrepFile {
		return 0, nil
	}
	r := bufio.NewReaderSize(f, 64<<10)
	if head, _ := r.Peek(8000); bytes.IndexByte(head, 0) >= 0 {
		return 0, nil
	}
	var out []string
	var before []string
	n, lineno, after, lastOut := 0, 0, 0, 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxGrepFile)
	for sc.Scan() {
		lineno++
		line := sc.Text()
		show := line
		if utf8.RuneCountInString(show) > maxGrepLine {
			show = string([]rune(show)[:maxGrepLine]) + " [line cut]"
		}
		if re.MatchString(line) {
			n++
			if content {
				if lastOut > 0 && lineno-len(before) > lastOut+1 {
					out = append(out, "--")
				}
				for i, b := range before {
					out = append(out, fmt.Sprintf("%s-%d-%s", p, lineno-len(before)+i, b))
				}
				before = before[:0]
				out = append(out, fmt.Sprintf("%s:%d:%s", p, lineno, show))
				lastOut = lineno
				after = ctxLines
			}
			continue
		}
		if !content {
			continue
		}
		if after > 0 {
			out = append(out, fmt.Sprintf("%s-%d-%s", p, lineno, show))
			lastOut = lineno
			after--
			continue
		}
		if ctxLines > 0 {
			before = append(before, show)
			if len(before) > ctxLines {
				before = before[1:]
			}
		}
	}
	return n, out
}

// capOutput keeps the start and the end of s, at most n bytes in all.
func capOutput(s string, n int) string {
	if len(s) <= n {
		return s
	}
	h := n * 2 / 3
	t := n - h
	head, tail := s[:h], s[len(s)-t:]
	for !utf8.ValidString(head) && len(head) > 0 {
		head = head[:len(head)-1]
	}
	for !utf8.ValidString(tail) && len(tail) > 0 {
		tail = tail[1:]
	}
	return fmt.Sprintf("%s\n[... %d bytes cut ...]\n%s", head, len(s)-len(head)-len(tail), tail)
}
