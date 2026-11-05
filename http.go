package main

// MCP's Streamable HTTP transport: one endpoint, a POST per message (or
// batch), answered as application/json. The server starts no streams of its
// own, so GET is refused (405), as the spec allows. Mcp-Session-Id is set at
// initialize; DELETE ends the session.

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
)

const maxBody = 64 << 20

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="9front-mcp"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// a browser page must not drive this server (DNS rebinding): any Origin
	// is refused unless allowed by -origin
	if o := r.Header.Get("Origin"); o != "" && !s.originAllowed(o) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	sid := r.Header.Get("Mcp-Session-Id")
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		if sid == "" || !s.endSession(sid) {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > maxBody {
		http.Error(w, "message too large", http.StatusRequestEntityTooLarge)
		return
	}
	msgs, batch, err := decode(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, parseError(err))
		return
	}
	init := false
	for _, m := range msgs {
		if m.Method == "initialize" {
			init = true
		}
	}
	if init {
		sid = s.newSession()
		w.Header().Set("Mcp-Session-Id", sid)
	} else if _, ok := s.session(sid); !ok {
		// the spec's answer to an unknown or ended session: the client
		// starts a new one
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	var resps []*rpcResponse
	for _, m := range msgs {
		if resp := s.handle(r.Context(), sid, m); resp != nil {
			resps = append(resps, resp)
		}
	}
	if len(resps) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if batch {
		writeJSON(w, http.StatusOK, resps)
	} else {
		writeJSON(w, http.StatusOK, resps[0])
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("marshal: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(b)
}

func (s *Server) authorized(r *http.Request) bool {
	if s.cfg.Token == "" {
		return true
	}
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(tok)), []byte(s.cfg.Token)) == 1
}

func (s *Server) originAllowed(o string) bool {
	for _, a := range s.cfg.Origins {
		if a == o || a == "*" {
			return true
		}
	}
	return false
}
