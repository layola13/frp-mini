// Package gotunnel is the Go port of frp-mini's tunnel multiplexing.
//
// Wire protocol (JSON text frames, both directions) is backward compatible
// with the TypeScript client/server, plus a streaming extension that fixes
// two productionlimitions of the original design:
//
//  1. The original client buffered the whole upstream body
//     (await r.arrayBuffer()) and answered with a single `res` message,
//     while the server killed every request after a hardcoded 25s
//     (UPSTREAM_TIMEOUT_MS). SSE therefore never streamed and any turn
//     slower than ~25s died with 504 "upstream timeout".
//  2. This port adds res_start/res_chunk/res_end: the client forwards
//     upstream bytes as they arrive, the server flushes each chunk to the
//     visitor immediately, and the deadline is configurable
//     (--upstream-timeout, default 5m). Legacy single-`res` clients keep
//     working: the server assembles them the old way.
package gotunnel

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// Message types on the agent control channel.
const (
	MsgRegister   = "register"
	MsgRegistered = "registered"
	MsgPing       = "ping"
	MsgPong       = "pong"
	MsgError      = "error"
	MsgReq        = "req"
	MsgRes        = "res" // legacy single-shot response
	MsgResStart   = "res_start"
	MsgResChunk   = "res_chunk"
	MsgResEnd     = "res_end"
	MsgResError   = "res_error"
	MsgWSOpen     = "ws_open"
	MsgWSData     = "ws_data"
	MsgWSClose    = "ws_close"
)

// MaxBody caps a single proxied body (same 10MB as the TS server).
const MaxBody = 10 * 1024 * 1024

// ChunkSize is the streaming granularity for res_chunk payloads.
const ChunkSize = 32 * 1024

// ReqMsg is server -> agent. Tunnel names the matched route (multi-name
// agents); omitted by legacy servers, in which case the client falls back
// to its first registered name.
type ReqMsg struct {
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	BodyB64 string            `json:"bodyB64,omitempty"`
	Tunnel  string            `json:"tunnel,omitempty"`
}

// ResMsg is agent -> server (legacy whole-body form).
type ResMsg struct {
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	BodyB64 string            `json:"bodyB64,omitempty"`
}

// ResStartMsg begins a streamed response.
type ResStartMsg struct {
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
}

// ResChunkMsg carries one body fragment (base64).
type ResChunkMsg struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Data string `json:"data"`
}

// ResEndMsg terminates a streamed response.
type ResEndMsg struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// ResErrorMsg aborts a streamed response (upstream failed mid-body).
type ResErrorMsg struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Error string `json:"error"`
}

// RegisterMsg is agent -> server on control-channel open.
type RegisterMsg struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Token string `json:"token,omitempty"`
}

// WSOpenMsg asks the agent to open an upstream websocket.
type WSOpenMsg struct {
	Type      string `json:"type"`
	ConnID    string `json:"connId"`
	Path      string `json:"path"`
	Protocols string `json:"protocols,omitempty"`
	Tunnel    string `json:"tunnel,omitempty"`
}

// WSDataMsg carries one websocket frame payload (base64 when binary).
type WSDataMsg struct {
	Type     string `json:"type"`
	ConnID   string `json:"connId"`
	Data     string `json:"data"`
	IsBinary bool   `json:"isBinary,omitempty"`
}

// WSCloseMsg closes a proxied websocket.
type WSCloseMsg struct {
	Type   string `json:"type"`
	ConnID string `json:"connId"`
	Code   int    `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidName reports whether a tunnel name is registrable.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// hopHeaders are stripped in both directions (same set as the TS server).
func hopHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, vs := range h {
		lk := strings.ToLower(k)
		switch lk {
		case "content-encoding", "content-length", "transfer-encoding", "connection":
			continue
		}
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out
}

// EncodeBody base64-encodes a body for the wire ("" when empty).
func EncodeBody(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// DecodeBody reverses EncodeBody.
func DecodeBody(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	return base64.StdEncoding.DecodeString(s)
}

// ResponseError formats the 504 body the old server produced, kept so
// clients see a familiar message when our own deadline fires.
func ResponseError(err error) string { return fmt.Sprintf("upstream error: %v", err) }
