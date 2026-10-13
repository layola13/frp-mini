package gotunnel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestValidName(t *testing.T) {
	for _, ok := range []string{"a", "colab1", "goproxy-test", "x-9", strings.Repeat("a", 32)} {
		if !ValidName(ok) {
			t.Errorf("want valid: %q", ok)
		}
	}
	for _, bad := range []string{"", "A", "a_b", "-x", "x!", strings.Repeat("a", 33)} {
		if ValidName(bad) {
			t.Errorf("want invalid: %q", bad)
		}
	}
	// NOTE: reserved names pass ValidName (reservation is checked
	// separately at register/enroll time, like the TS server).
	if ValidName("") || ValidName("A B") {
		t.Errorf("empty/space must be invalid")
	}
}

func TestHopHeaders(t *testing.T) {
	h := http.Header{
		"Content-Type":      {"text/event-stream"},
		"Content-Length":    {"10"},
		"Connection":        {"keep-alive"},
		"X-Working-Dir":     {"d:\\x"},
		"Transfer-Encoding": {"chunked"},
	}
	got := hopHeaders(h)
	if _, ok := got["Content-Length"]; ok {
		t.Errorf("content-length must be stripped")
	}
	if _, ok := got["Connection"]; ok {
		t.Errorf("connection must be stripped")
	}
	if got["Content-Type"] != "text/event-stream" || got["X-Working-Dir"] != `d:\x` {
		t.Errorf("kept headers wrong: %v", got)
	}
}

func TestBodyRoundtrip(t *testing.T) {
	if EncodeBody(nil) != "" {
		t.Errorf("empty must encode empty")
	}
	raw := []byte("hello \x00\xff binary")
	enc := EncodeBody(raw)
	dec, err := DecodeBody(enc)
	if err != nil || string(dec) != string(raw) {
		t.Errorf("roundtrip fail: %v", err)
	}
	if _, err := DecodeBody("!!!not-base64!!!"); err == nil {
		t.Errorf("bad base64 must error")
	}
}

// testAgent dials a Server's control channel and registers name.
func testAgent(t *testing.T, srvURL, name string) *websocket.Conn {
	t.Helper()
	wsURL := strings.Replace(srvURL, "http://", "ws://", 1) + "/__tunnel"
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WriteJSON(RegisterMsg{Type: MsgRegister, Name: name}); err != nil {
		t.Fatal(err)
	}
	// consume ping/registered frames until registered arrives
	deadline := time.Now().Add(5 * time.Second)
	for {
		_ = c.SetReadDeadline(deadline)
		_, raw, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		var base struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &base)
		if base.Type == MsgRegistered {
			return c
		}
	}
}

func testServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	opt := DefaultOptions()
	opt.DBFile = t.TempDir() + "/routes.json"
	opt.UpstreamTimeout = 5 * time.Second
	srv := NewServer(opt)
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)
	return srv, httpSrv
}

func TestLegacyResCompat(t *testing.T) {
	_, httpSrv := testServer(t)
	agent := testAgent(t, httpSrv.URL, "legacy1")
	defer agent.Close()

	done := make(chan ReqMsg, 1)
	go func() {
		for {
			var m ReqMsg
			if err := agent.ReadJSON(&m); err != nil {
				return
			}
			if m.Type == MsgReq {
				done <- m
				return
			}
		}
	}()
	go func() {
		m := <-done
		body, _ := DecodeBody(m.BodyB64)
		_ = agent.WriteJSON(ResMsg{Type: MsgRes, ID: m.ID, Status: 201,
			Headers: map[string]string{"content-type": "text/plain", "x-echo": string(body)},
			BodyB64: EncodeBody([]byte("hi-" + m.Path))})
	}()

	resp, err := http.Post(httpSrv.URL+"/legacy1/hello", "text/plain", strings.NewReader("ping"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 201 || string(raw) != "hi-/hello" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, raw)
	}
	if resp.Header.Get("x-echo") != "ping" {
		t.Errorf("request body not forwarded: %q", resp.Header.Get("x-echo"))
	}
}

func TestChunkedStreamingTTFB(t *testing.T) {
	_, httpSrv := testServer(t)
	agent := testAgent(t, httpSrv.URL, "stream1")
	defer agent.Close()

	go func() {
		for {
			var m ReqMsg
			if err := agent.ReadJSON(&m); err != nil {
				return
			}
			if m.Type != MsgReq {
				continue
			}
			_ = agent.WriteJSON(ResStartMsg{Type: MsgResStart, ID: m.ID, Status: 200,
				Headers: map[string]string{"content-type": "text/event-stream"}})
			_ = agent.WriteJSON(ResChunkMsg{Type: MsgResChunk, ID: m.ID, Data: EncodeBody([]byte("data: one\n\n"))})
			time.Sleep(300 * time.Millisecond) // slow producer
			_ = agent.WriteJSON(ResChunkMsg{Type: MsgResChunk, ID: m.ID, Data: EncodeBody([]byte("data: two\n\n"))})
			_ = agent.WriteJSON(ResEndMsg{Type: MsgResEnd, ID: m.ID})
			return
		}
	}()

	start := time.Now()
	resp, err := http.Get(httpSrv.URL + "/stream1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 11) // "data: one\n\n" is 11 bytes
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	ttfb := time.Since(start)
	if string(buf) != "data: one\n\n" {
		t.Fatalf("first chunk = %q", buf)
	}
	if ttfb > 250*time.Millisecond {
		t.Fatalf("first byte must stream immediately, took %s", ttfb)
	}
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "data: two\n\n" {
		t.Fatalf("tail = %q", rest)
	}
}

func TestUpstreamTimeoutConfigurable(t *testing.T) {
	opt := DefaultOptions()
	opt.DBFile = t.TempDir() + "/routes.json"
	opt.UpstreamTimeout = 150 * time.Millisecond
	srv := NewServer(opt)
	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()
	agent := testAgent(t, httpSrv.URL, "slow1")
	defer agent.Close()
	go func() {
		for {
			var m ReqMsg
			if err := agent.ReadJSON(&m); err != nil {
				return
			}
			// never answer: let the deadline fire
			_ = m
			return
		}
	}()
	start := time.Now()
	resp, err := http.Get(httpSrv.URL + "/slow1/x")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	el := time.Since(start)
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("want 504, got %d (%s)", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "upstream timeout") {
		t.Fatalf("body = %q", raw)
	}
	if el < 150*time.Millisecond || el > 3*time.Second {
		t.Fatalf("deadline not honored: %s", el)
	}
}

func TestRegisterBadName(t *testing.T) {
	_, httpSrv := testServer(t)
	wsURL := strings.Replace(httpSrv.URL, "http://", "ws://", 1) + "/__tunnel"
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.WriteJSON(RegisterMsg{Type: MsgRegister, Name: "BAD NAME!"})
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, raw, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "bad name") {
		t.Fatalf("want bad-name error, got %s", raw)
	}
}

func TestStaticBind(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("static:" + r.URL.RequestURI()))
	}))
	defer up.Close()
	opt := DefaultOptions()
	opt.DBFile = t.TempDir() + "/routes.json"
	opt.StaticRoot = ""
	opt.StaticBinds = map[string]string{"fixed": up.URL}
	srv := NewServer(opt)
	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()
	resp, err := http.Get(httpSrv.URL + "/fixed/deep?q=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if string(raw) != "static:/deep?q=1" {
		t.Fatalf("static bind path wrong: %q", raw)
	}
}

func TestRewriteHTML(t *testing.T) {
	in := `<html><head><title>t</title></head><body><a href="/assets/a.js">x</a><img src='/i.png'></body></html>`
	out := rewriteHTML(in, "n1")
	for _, want := range []string{`<base href="/n1/" />`, `href="/n1/assets/a.js"`, `src='/n1/i.png'`, `window.__CODEX_UI_WEBSOCKET_URL__ = "/n1/ws"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestMultiNameOneConn(t *testing.T) {
	srv, httpSrv := testServer(t)
	wsURL := strings.Replace(httpSrv.URL, "http://", "ws://", 1) + "/__tunnel"
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// One control connection registers two names.
	for _, n := range []string{"multi-a", "multi-b"} {
		if err := c.WriteJSON(RegisterMsg{Type: MsgRegister, Name: n}); err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			var base struct {
				Type string `json:"type"`
			}
			if err := c.ReadJSON(&base); err != nil {
				t.Fatalf("register %s: %v", n, err)
			}
			if base.Type == MsgRegistered {
				break
			}
		}
	}
	srv.mu.Lock()
	_, hasA := srv.agents["multi-a"]
	_, hasB := srv.agents["multi-b"]
	srv.mu.Unlock()
	if !hasA || !hasB {
		t.Fatalf("both names must be live: a=%v b=%v", hasA, hasB)
	}
	// Both names serve: answer reqs with the tunnel field echoed.
	go func() {
		for {
			var m ReqMsg
			if err := c.ReadJSON(&m); err != nil {
				return
			}
			if m.Type != MsgReq {
				continue
			}
			_ = c.WriteJSON(ResMsg{Type: MsgRes, ID: m.ID, Status: 200,
				Headers: map[string]string{"content-type": "text/plain"},
				BodyB64: EncodeBody([]byte("via=" + m.Tunnel + ":" + m.Path))})
		}
	}()
	for name, want := range map[string]string{"multi-a": "via=multi-a:/x", "multi-b": "via=multi-b:/x"} {
		resp, err := http.Get(httpSrv.URL + "/" + name + "/x")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(raw) != want {
			t.Errorf("%s -> %q, want %q", name, raw, want)
		}
	}
	// Closing the one conn drops both names.
	_ = c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		srv.mu.Lock()
		_, hasA := srv.agents["multi-a"]
		_, hasB := srv.agents["multi-b"]
		srv.mu.Unlock()
		if !hasA && !hasB {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("names must drop after conn close: a=%v b=%v", hasA, hasB)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestConcurrentControlWrites hammers one agent with parallel HTTP turns
// plus re-registers on the same control conn. Run with -race: any
// concurrent control-frame write corrupts the stream (the legacy-client
// flap) and the detector flags it.
func TestConcurrentControlWrites(t *testing.T) {
	_, httpSrv := testServer(t)
	wsURL := strings.Replace(httpSrv.URL, "http://", "ws://", 1) + "/__tunnel"
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.WriteJSON(RegisterMsg{Type: MsgRegister, Name: "race1"}); err != nil {
		t.Fatal(err)
	}
	// Dedicated answer loop (single reader) + parallel turns.
	// Test-side writes share one mutex, mirroring production send()
	// discipline, so -race only flags server-side violations.
	var wmu sync.Mutex
	twrite := func(v any) error {
		wmu.Lock()
		defer wmu.Unlock()
		return c.WriteJSON(v)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			var m ReqMsg
			c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			if err := c.ReadJSON(&m); err != nil {
				select {
				case <-stop:
					return
				default:
				}
				continue
			}
			if m.Type != MsgReq {
				continue
			}
			_ = twrite(ResMsg{Type: MsgRes, ID: m.ID, Status: 200,
				Headers: map[string]string{"content-type": "text/plain"},
				BodyB64: EncodeBody([]byte("ok"))})
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%4 == 0 {
				// Concurrent re-register on the same conn (exercises the
				// register-vs-request write interleave that dropped clients).
				_ = twrite(RegisterMsg{Type: MsgRegister, Name: "race1"})
			}
			resp, err := http.Get(httpSrv.URL + "/race1/t")
			if err != nil {
				t.Errorf("turn %d: %v", i, err)
				return
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(raw) != "ok" {
				t.Errorf("turn %d = %q", i, raw)
			}
		}(i)
	}
	wg.Wait()
}
