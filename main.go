// 9front-mcp is an MCP server for 9front (Plan 9): it gives an agent such as
// Claude Code, running on another machine, tools to read, write, edit and
// search files and to run rc commands on the 9front system.
//
// usage: 9front-mcp [-http addr] [-stdio] [-dir dir] [-token tok | -tokenfile f | -noauth]
//
//	[-tlscert f -tlskey f] [-origin o]... [-shell rc] [-deny file] [-disks] [-v]
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Dir       string
	Shell     string
	Token     string
	Origins   []string
	Verbose   bool
	Deny      []denyRule
	Guard     string   // rc lines run before each command (diskGuard)
	DiskPaths []string // what write and edit must not touch
}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(s string) error { *l = append(*l, s); return nil }

func main() {
	log.SetFlags(0)
	log.SetPrefix("9front-mcp: ")
	var (
		addr      = flag.String("http", "", "serve MCP over HTTP at `address` (tcp!*!8090 or :8090); the endpoint is /mcp")
		stdio     = flag.Bool("stdio", false, "serve MCP on standard input and output")
		dir       = flag.String("dir", "", "`directory` relative paths start from (the current one by default)")
		token     = flag.String("token", "", "bearer `token` HTTP clients must send (default $MCPTOKEN; else one is made up and printed)")
		tokenfile = flag.String("tokenfile", "", "read the bearer token from `file`")
		noauth    = flag.Bool("noauth", false, "HTTP without a token: anyone who reaches the port runs commands")
		tlscert   = flag.String("tlscert", "", "serve HTTPS with this certificate `file` (PEM)")
		tlskey    = flag.String("tlskey", "", "and this key `file` (PEM)")
		shell     = flag.String("shell", "", "the shell the rc tool runs, as shell -c cmd (/bin/rc)")
		verbose   = flag.Bool("v", false, "log the tool calls")
		disks     = flag.Bool("disks", false, "let the tools at the disk devices (#S, /dev/sd*); by default rc commands lose them (chdev) and write/edit refuse them")
		deny      = flag.String("deny", "", "refuse rc commands and write/edit paths that match the rules in `file` (see deny.example)")
		origins   listFlag
	)
	flag.Var(&origins, "origin", "allow requests that carry this `Origin` header (browsers); repeatable, * for any")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: 9front-mcp [-http addr | -stdio] [options]\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() > 0 || (*addr == "") == !*stdio {
		flag.Usage()
		os.Exit(2)
	}

	cfg := &Config{Dir: *dir, Shell: *shell, Origins: origins, Verbose: *verbose}
	if cfg.Dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
		cfg.Dir = wd
	}
	if fi, err := os.Stat(cfg.Dir); err != nil || !fi.IsDir() {
		log.Fatalf("-dir %s: not a directory", cfg.Dir)
	}
	if cfg.Shell == "" {
		cfg.Shell = defaultShell
		if _, err := exec.LookPath(cfg.Shell); err != nil {
			cfg.Shell = "/bin/sh"
		}
	}
	if *deny != "" {
		rules, err := loadDeny(*deny)
		if err != nil {
			log.Fatal(err)
		}
		cfg.Deny = rules
	}
	if !*disks {
		guard, paths, err := diskGuard()
		if err != nil {
			log.Fatalf("disk guard: %v (-disks runs without it)", err)
		}
		cfg.Guard, cfg.DiskPaths = guard, paths
	}
	srv := newServer(cfg)

	if *stdio {
		serveStdio(srv, os.Stdin, os.Stdout)
		return
	}

	switch {
	case *noauth:
	case *tokenfile != "":
		b, err := os.ReadFile(*tokenfile)
		if err != nil {
			log.Fatal(err)
		}
		cfg.Token = strings.TrimSpace(string(b))
	case *token != "":
		cfg.Token = *token
	case os.Getenv("MCPTOKEN") != "":
		cfg.Token = strings.TrimSpace(os.Getenv("MCPTOKEN"))
	default:
		cfg.Token = newID()
		log.Printf("token %s", cfg.Token)
	}
	if !*noauth && cfg.Token == "" {
		log.Fatal("the token is empty")
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", srv)
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second}
	ln, err := net.Listen("tcp", goAddr(*addr))
	if err != nil {
		log.Fatal(err)
	}
	scheme := "http"
	if *tlscert != "" {
		scheme = "https"
	}
	log.Printf("serving %s://%s/mcp, rc %s in %s, %d deny rules, disks %s", scheme, ln.Addr(), cfg.Shell, cfg.Dir,
		len(cfg.Deny), map[bool]string{true: "guarded", false: "open"}[cfg.Guard != ""])
	if *tlscert != "" {
		err = hs.ServeTLS(ln, *tlscert, *tlskey)
	} else {
		err = hs.Serve(ln)
	}
	log.Fatal(err)
}

// goAddr turns a Plan 9 dial string (tcp!*!8090, tcp!host!port) into Go's
// host:port.
func goAddr(a string) string {
	f := strings.Split(a, "!")
	switch len(f) {
	case 2:
		f = []string{"tcp", f[0], f[1]}
		fallthrough
	case 3:
		host := f[1]
		if host == "*" {
			host = ""
		}
		return net.JoinHostPort(host, f[2])
	}
	return a
}

// serveStdio reads one JSON-RPC message (or batch) a line and writes the
// answers a line each; requests run concurrently, so that one can be
// cancelled while it runs.
func serveStdio(srv *Server, in *os.File, out *os.File) {
	var mu sync.Mutex
	w := bufio.NewWriter(out)
	send := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			log.Print(err)
			return
		}
		mu.Lock()
		w.Write(b)
		w.WriteByte('\n')
		w.Flush()
		mu.Unlock()
	}
	var wg sync.WaitGroup
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), maxBody)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		msgs, batch, err := decode(line)
		if err != nil {
			send(parseError(err))
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			var resps []*rpcResponse
			for _, m := range msgs {
				if r := srv.handle(context.Background(), "", m); r != nil {
					resps = append(resps, r)
				}
			}
			switch {
			case len(resps) == 0:
			case batch:
				send(resps)
			default:
				send(resps[0])
			}
		}()
	}
	if err := sc.Err(); err != nil {
		log.Print(err)
	}
	wg.Wait()
}
