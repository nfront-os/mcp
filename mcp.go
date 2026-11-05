package main

// The MCP side: JSON-RPC 2.0 messages, the initialize handshake, tools/list
// and tools/call, cancellation. Both transports (http.go, stdio in main.go)
// hand each message to (*Server).handle.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sync"
)

const serverName = "9front-mcp"
const serverVersion = "0.1"

// Protocol versions this server speaks, newest first. It answers a client's
// version when it is one of these, else the newest.
var protocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const instructions = `These tools work on a 9front (Plan 9) system, not on the machine Claude Code runs on.
- rc runs a command in rc(1), Plan 9's shell: no bash, no sh syntax. if(...) and for(i in ...) take
  braces, quoting is '...' only ('' inside is a quote), $status is a string ('' is success),
  >[2=1] sends stderr to stdout, ` + "`" + `{cmd} substitutes. Each call is a fresh rc in its own
  name space and note group: cd, bind and variables do not carry over; use the dir argument.
- The tools there are Plan 9's: mk not make, walk instead of find, grep has no -r (use the
  grep tool), ls -l, cat, sed, awk, 6c/8c/7c or gcc where installed, git (9front's git/).
- Use read, edit and write for files, glob and grep to search, not cat/sed/grep in rc.
- Synthetic files (/dev, /proc, /net, /srv, /env) can block on read; glob and grep skip them
  unless they are the path given.`

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	errParse          = -32700
	errInvalidRequest = -32600
	errMethodNotFound = -32601
	errInvalidParams  = -32602
)

// Session is what the server keeps per client: the files it has read, so
// that write does not overwrite a file the client has not seen.
type Session struct {
	mu    sync.Mutex
	reads map[string]fileStamp
}

func newSession() *Session {
	return &Session{reads: map[string]fileStamp{}}
}

type Server struct {
	cfg *Config

	mu       sync.Mutex
	sessions map[string]*Session
	inflight map[string]context.CancelFunc
}

func newServer(cfg *Config) *Server {
	return &Server{cfg: cfg, sessions: map[string]*Session{}, inflight: map[string]context.CancelFunc{}}
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (s *Server) newSession() string {
	id := newID()
	s.mu.Lock()
	s.sessions[id] = newSession()
	s.mu.Unlock()
	return id
}

// session returns the session of that id; "" is the session of a client that
// sends none (stdio, or an HTTP client that ignores Mcp-Session-Id).
func (s *Server) session(id string) (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.sessions[id]
	if !ok && id == "" {
		ss = newSession()
		s.sessions[""] = ss
		ok = true
	}
	return ss, ok
}

func (s *Server) endSession(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.sessions[id]
	delete(s.sessions, id)
	return ok
}

// handle answers one message. It returns nil for a notification or a
// response, which get no answer.
func (s *Server) handle(ctx context.Context, sid string, m *rpcMessage) *rpcResponse {
	if len(m.ID) == 0 || m.Method == "" {
		if m.Method != "" {
			s.notification(sid, m)
		}
		return nil
	}
	resp := &rpcResponse{JSONRPC: "2.0", ID: m.ID}
	if m.JSONRPC != "2.0" {
		resp.Error = &rpcError{errInvalidRequest, "jsonrpc must be \"2.0\""}
		return resp
	}
	key := sid + "/" + string(m.ID)
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.inflight[key] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inflight, key)
		s.mu.Unlock()
		cancel()
	}()

	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(m.Params, &p)
		v := protocolVersions[0]
		for _, pv := range protocolVersions {
			if pv == p.ProtocolVersion {
				v = pv
			}
		}
		resp.Result = map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
			"instructions":    instructions,
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": toolList()}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			resp.Error = &rpcError{errInvalidParams, err.Error()}
			break
		}
		t := findTool(p.Name)
		if t == nil {
			resp.Error = &rpcError{errInvalidParams, "unknown tool " + p.Name}
			break
		}
		ss, ok := s.session(sid)
		if !ok {
			ss = newSession()
		}
		if s.cfg.Verbose {
			log.Printf("call %s %s", p.Name, truncate(string(p.Arguments), 200))
		}
		resp.Result = s.call(ctx, ss, t, p.Arguments)
	default:
		resp.Error = &rpcError{errMethodNotFound, "method not found: " + m.Method}
	}
	return resp
}

func (s *Server) notification(sid string, m *rpcMessage) {
	if m.Method != "notifications/cancelled" {
		return
	}
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(m.Params, &p) != nil {
		return
	}
	s.mu.Lock()
	cancel := s.inflight[sid+"/"+string(p.RequestID)]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

type toolResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// call runs a tool. File tools run in a goroutine of their own: a read of a
// synthetic file can block in the kernel, and the call then returns when it
// is cancelled while the read stays behind.
func (s *Server) call(ctx context.Context, ss *Session, t *Tool, args json.RawMessage) toolResult {
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	type out struct {
		text string
		err  error
	}
	ch := make(chan out, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- out{"", fmt.Errorf("panic: %v", r)}
			}
		}()
		text, err := t.Run(ctx, s.cfg, ss, args)
		ch <- out{text, err}
	}()
	var o out
	select {
	case o = <-ch:
	case <-ctx.Done():
		o = out{"", fmt.Errorf("cancelled")}
	}
	if o.err != nil {
		text := o.err.Error()
		if o.text != "" {
			text = o.text + "\n" + text
		}
		return toolResult{Content: []textContent{{"text", text}}, IsError: true}
	}
	return toolResult{Content: []textContent{{"text", o.text}}}
}

// decode reads the JSON-RPC message or batch in b.
func decode(b []byte) (msgs []*rpcMessage, batch bool, err error) {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\r' || b[0] == '\n') {
		b = b[1:]
	}
	if len(b) > 0 && b[0] == '[' {
		err = json.Unmarshal(b, &msgs)
		return msgs, true, err
	}
	var m rpcMessage
	if err = json.Unmarshal(b, &m); err != nil {
		return nil, false, err
	}
	return []*rpcMessage{&m}, false, nil
}

func parseError(err error) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{errParse, err.Error()}}
}
