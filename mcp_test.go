package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type client struct {
	t   *testing.T
	cfg *Config
	url string
	tok string
	sid string
	id  int
}

func newTestServer(t *testing.T) (*client, string) {
	dir := t.TempDir()
	shell := "/bin/sh"
	cfg := &Config{Dir: dir, Shell: shell, Token: "secret"}
	ts := httptest.NewServer(http.HandlerFunc(newServer(cfg).ServeHTTP))
	t.Cleanup(ts.Close)
	c := &client{t: t, cfg: cfg, url: ts.URL, tok: "secret"}
	return c, dir
}

func (c *client) post(body string) (*http.Response, []byte) {
	req, _ := http.NewRequest("POST", c.url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.tok != "" {
		req.Header.Set("Authorization", "Bearer "+c.tok)
	}
	if c.sid != "" {
		req.Header.Set("Mcp-Session-Id", c.sid)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	return resp, b.Bytes()
}

func (c *client) rpc(method string, params any) map[string]any {
	c.id++
	p, _ := json.Marshal(params)
	resp, b := c.post(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, c.id, method, p))
	if resp.StatusCode != 200 {
		c.t.Fatalf("%s: %s %s", method, resp.Status, b)
	}
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		c.sid = s
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		c.t.Fatalf("%s: %v: %s", method, err, b)
	}
	return m
}

func (c *client) init() {
	m := c.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "0"}})
	r := m["result"].(map[string]any)
	if r["protocolVersion"] != "2025-06-18" {
		c.t.Fatalf("protocolVersion %v", r["protocolVersion"])
	}
	if c.sid == "" {
		c.t.Fatal("no Mcp-Session-Id")
	}
	resp, _ := c.post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if resp.StatusCode != http.StatusAccepted {
		c.t.Fatalf("initialized: %s", resp.Status)
	}
}

// call returns the tool's text and whether it is an error.
func (c *client) call(name string, args map[string]any) (string, bool) {
	m := c.rpc("tools/call", map[string]any{"name": name, "arguments": args})
	if e, ok := m["error"]; ok {
		c.t.Fatalf("%s: rpc error %v", name, e)
	}
	r := m["result"].(map[string]any)
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	isErr, _ := r["isError"].(bool)
	return text, isErr
}

func (c *client) ok(name string, args map[string]any) string {
	text, isErr := c.call(name, args)
	if isErr {
		c.t.Fatalf("%s %v: %s", name, args, text)
	}
	return text
}

func (c *client) fails(name string, args map[string]any, want string) {
	text, isErr := c.call(name, args)
	if !isErr || !strings.Contains(text, want) {
		c.t.Fatalf("%s %v: want an error with %q, got %v %q", name, args, want, isErr, text)
	}
}

func TestProtocol(t *testing.T) {
	c, _ := newTestServer(t)

	// no token, wrong token
	c.tok = ""
	if resp, _ := c.post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`); resp.StatusCode != 401 {
		t.Fatalf("no token: %s", resp.Status)
	}
	c.tok = "wrong"
	if resp, _ := c.post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`); resp.StatusCode != 401 {
		t.Fatalf("wrong token: %s", resp.Status)
	}
	c.tok = "secret"

	c.init()
	if m := c.rpc("ping", nil); m["result"] == nil {
		t.Fatalf("ping: %v", m)
	}
	m := c.rpc("tools/list", nil)
	var names []string
	for _, tl := range m["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if strings.Join(names, " ") != "read write edit glob grep rc" {
		t.Fatalf("tools %v", names)
	}
	if m := c.rpc("nosuch", nil); m["error"] == nil {
		t.Fatalf("unknown method: %v", m)
	}
	if m := c.rpc("tools/call", map[string]any{"name": "nosuch"}); m["error"] == nil {
		t.Fatalf("unknown tool: %v", m)
	}

	// a batch
	resp, b := c.post(`[{"jsonrpc":"2.0","id":10,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/x"},{"jsonrpc":"2.0","id":11,"method":"ping"}]`)
	var batch []map[string]any
	if resp.StatusCode != 200 || json.Unmarshal(b, &batch) != nil || len(batch) != 2 {
		t.Fatalf("batch: %s %s", resp.Status, b)
	}

	// GET has no stream; an unknown session is 404; DELETE ends one
	req, _ := http.NewRequest("GET", c.url, nil)
	req.Header.Set("Authorization", "Bearer secret")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 405 {
		t.Fatalf("GET: %s", resp.Status)
	}
	sid := c.sid
	c.sid = "nosuch"
	if resp, _ := c.post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`); resp.StatusCode != 404 {
		t.Fatalf("unknown session: %s", resp.Status)
	}
	req, _ = http.NewRequest("DELETE", c.url, nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Mcp-Session-Id", sid)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 200 {
		t.Fatalf("DELETE: %s", resp.Status)
	}
	c.sid = sid
	if resp, _ := c.post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`); resp.StatusCode != 404 {
		t.Fatalf("ended session: %s", resp.Status)
	}

	// a browser's request
	req, _ = http.NewRequest("POST", c.url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Origin", "http://evil.example")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 403 {
		t.Fatalf("Origin: %s", resp.Status)
	}
}

func TestFiles(t *testing.T) {
	c, dir := newTestServer(t)
	c.init()

	if s := c.ok("write", map[string]any{"path": "sub/a.c", "content": "one\ntwo\nthree\ntwo\n"}); !strings.Contains(s, "18 bytes") {
		t.Fatalf("write: %s", s)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "sub/a.c"))
	if string(b) != "one\ntwo\nthree\ntwo\n" {
		t.Fatalf("content %q", b)
	}
	if s := c.ok("read", map[string]any{"path": "sub/a.c"}); s != "     1\tone\n     2\ttwo\n     3\tthree\n     4\ttwo\n" {
		t.Fatalf("read: %q", s)
	}
	if s := c.ok("read", map[string]any{"path": filepath.Join(dir, "sub/a.c"), "offset": 2, "limit": 1}); !strings.HasPrefix(s, "     2\ttwo\n[lines 2-2 of 4") {
		t.Fatalf("read offset: %q", s)
	}
	if s := c.ok("read", map[string]any{"path": "."}); s != "sub/\n" {
		t.Fatalf("read dir: %q", s)
	}

	c.fails("edit", map[string]any{"path": "sub/a.c", "old_string": "two", "new_string": "2"}, "occurs 2 times")
	c.fails("edit", map[string]any{"path": "sub/a.c", "old_string": "four", "new_string": "4"}, "not found")
	if s := c.ok("edit", map[string]any{"path": "sub/a.c", "old_string": "three", "new_string": "3"}); !strings.Contains(s, "1 replacement") || !strings.Contains(s, "     3\t3\n") {
		t.Fatalf("edit: %q", s)
	}
	if s := c.ok("edit", map[string]any{"path": "sub/a.c", "old_string": "two", "new_string": "2", "replace_all": true}); !strings.Contains(s, "2 replacement") {
		t.Fatalf("edit all: %q", s)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "sub/a.c"))
	if string(b) != "one\n2\n3\n2\n" {
		t.Fatalf("after edit %q", b)
	}

	// overwriting needs a read; a change behind the session's back is caught
	os.WriteFile(filepath.Join(dir, "b.h"), []byte("x\n"), 0644)
	c.fails("write", map[string]any{"path": "b.h", "content": "y\n"}, "read it before")
	c.ok("read", map[string]any{"path": "b.h"})
	c.ok("write", map[string]any{"path": "b.h", "content": "y\n"})
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(filepath.Join(dir, "b.h"), []byte("zz\n"), 0644)
	c.fails("edit", map[string]any{"path": "b.h", "old_string": "zz", "new_string": "w"}, "changed since")

	os.WriteFile(filepath.Join(dir, "bin"), []byte("a\x00b"), 0644)
	if s := c.ok("read", map[string]any{"path": "bin"}); !strings.Contains(s, "binary file") {
		t.Fatalf("binary: %q", s)
	}
}

func TestSearch(t *testing.T) {
	c, dir := newTestServer(t)
	c.init()
	for _, f := range []string{"x/a.c", "x/y/b.c", "x/y/c.h", "z.txt", ".git/HEAD.c"} {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0755)
		os.WriteFile(filepath.Join(dir, f), []byte("int main;\nfoo bar\nFOO\nend\n"), 0644)
	}
	rel := func(s string) string { return strings.ReplaceAll(s, dir+"/", "") }
	lines := func(s string) string {
		l := strings.Fields(rel(s))
		return strings.Join(sortStrings(l), " ")
	}
	if s := lines(c.ok("glob", map[string]any{"pattern": "**/*.c"})); s != "x/a.c x/y/b.c" {
		t.Fatalf("glob **: %q", s)
	}
	if s := lines(c.ok("glob", map[string]any{"pattern": "x/y/*.{c,h}"})); s != "x/y/b.c x/y/c.h" {
		t.Fatalf("glob braces: %q", s)
	}
	if s := lines(c.ok("glob", map[string]any{"pattern": dir + "/x/*.c"})); s != "x/a.c" {
		t.Fatalf("glob absolute: %q", s)
	}
	if s := c.ok("glob", map[string]any{"pattern": "*.nope"}); s != "no files match" {
		t.Fatalf("glob none: %q", s)
	}
	if s := lines(c.ok("grep", map[string]any{"pattern": "foo", "glob": "*.c"})); s != "x/a.c x/y/b.c" {
		t.Fatalf("grep files: %q", s)
	}
	if s := rel(c.ok("grep", map[string]any{"pattern": "foo", "path": "z.txt", "output_mode": "count", "ignore_case": true})); s != "z.txt:2\n" {
		t.Fatalf("grep count: %q", s)
	}
	if s := rel(c.ok("grep", map[string]any{"pattern": "^FOO", "path": "x/a.c", "output_mode": "content", "context": 1})); s != "x/a.c-2-foo bar\nx/a.c:3:FOO\nx/a.c-4-end\n" {
		t.Fatalf("grep content: %q", s)
	}
	c.fails("grep", map[string]any{"pattern": "(("}, "missing")
}

func sortStrings(l []string) []string {
	for i := range l {
		for j := i + 1; j < len(l); j++ {
			if l[j] < l[i] {
				l[i], l[j] = l[j], l[i]
			}
		}
	}
	return l
}

func TestRc(t *testing.T) {
	c, dir := newTestServer(t)
	c.init()
	os.Mkdir(filepath.Join(dir, "d"), 0755)
	if s := c.ok("rc", map[string]any{"command": "pwd; echo err >&2", "dir": "d"}); s != filepath.Join(dir, "d")+"\nerr\n" {
		t.Fatalf("rc: %q", s)
	}
	if s := c.ok("rc", map[string]any{"command": "exit 3"}); !strings.Contains(s, "status: exit 3") {
		t.Fatalf("rc status: %q", s)
	}
	start := time.Now()
	text, isErr := c.call("rc", map[string]any{"command": "echo before; sleep 30 & sleep 30", "timeout": 1})
	if !isErr || !strings.Contains(text, "timed out") || !strings.Contains(text, "before") || time.Since(start) > 10*time.Second {
		t.Fatalf("rc timeout: %v %q after %v", isErr, text, time.Since(start))
	}
	if s := c.ok("rc", map[string]any{"command": "head -c 100000 /dev/zero | tr '\\0' x"}); !strings.Contains(s, "bytes cut") || len(s) > maxRcOutput+100 {
		t.Fatalf("rc cut: %d bytes", len(s))
	}
}

func TestGoAddr(t *testing.T) {
	for in, want := range map[string]string{"tcp!*!8090": ":8090", "*!8090": ":8090", "tcp!10.0.2.15!80": "10.0.2.15:80", ":8090": ":8090"} {
		if got := goAddr(in); got != want {
			t.Errorf("goAddr(%q) = %q, want %q", in, got, want)
		}
	}
}
