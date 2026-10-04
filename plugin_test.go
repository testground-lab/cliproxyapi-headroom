package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func setup(t *testing.T, url string) {
	t.Helper()
	setupConfig(t, url, "")
}
func setupConfig(t *testing.T, url, extra string) {
	t.Helper()
	y := []byte("endpoint: " + url + "\nmin_chars: 10\ntimeout_ms: 1000\n" + extra)
	r, _ := json.Marshal(map[string]any{"config_yaml": y})
	if e := configure(r); e != nil {
		t.Fatal(e)
	}
}
func TestProtocolsPreserveEnvelope(t *testing.T) {
	long := strings.Repeat("log entry ", 100)
	cases := []string{
		`{"model":"x","seed":9007199254740993,"messages":[{"role":"system","content":"INSTRUCTIONS"},{"role":"tool","tool_call_id":"real-id","content":"LONG"}],"stream":true}`,
		`{"model":"x","system":"INSTRUCTIONS","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"real-id","content":[{"type":"text","text":"LONG"},{"type":"image","source":{"data":"IMAGE"}}]}]}]}`,
		`{"model":"x","instructions":"INSTRUCTIONS","previous_response_id":"prev","input":[{"type":"function_call_output","call_id":"real-id","output":"LONG"},{"type":"reasoning","encrypted_content":"SECRET"}]}`,
		`{"contents":[{"parts":[{"functionResponse":{"name":"shell","response":{"output":"LONG","count":42}}}]}]}`,
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		if r.Header.Get("Authorization") != "" {
			t.Error("unexpected credentials")
		}
		cfg := b["config"].(map[string]any)
		if cfg["mode"] != "lossy_inline" {
			t.Error("unsafe mode")
		}
		msgs := b["messages"].([]any)
		for _, m := range msgs {
			m.(map[string]any)["content"] = "short"
		}
		json.NewEncoder(w).Encode(map[string]any{"messages": msgs, "ccr_hashes": []string{}})
	}))
	defer s.Close()
	setup(t, s.URL)
	for _, c := range cases {
		raw := []byte(strings.ReplaceAll(c, "LONG", long))
		got, err := compressBody(interceptRequest{Body: raw, Model: "x"}, settings.Load())
		if err != nil {
			t.Fatal(err)
		}
		want := []byte(strings.ReplaceAll(c, "LONG", "short"))
		var a, b any
		da := json.NewDecoder(bytes.NewReader(got))
		da.UseNumber()
		da.Decode(&a)
		db := json.NewDecoder(bytes.NewReader(want))
		db.UseNumber()
		db.Decode(&b)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("envelope changed: %s", got)
		}
	}
}
func TestFailOpen(t *testing.T) {
	for _, response := range []string{`bad`, `{"messages":[]}`, `{"messages":[],"ccr_hashes":["hash"]}`, `{"messages":[{"role":"tool","tool_call_id":"WRONG","content":"short"}]}`, `{"compression_skipped":true}`, `{"messages":[{"role":"tool","tool_call_id":"headroom_0","content":""}]}`} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(response)) }))
		setup(t, s.URL)
		req, _ := json.Marshal(interceptRequest{Body: []byte(`{"messages":[{"role":"tool","content":"long enough to compress"}]}`)})
		got, e := handleMethod("request.intercept_before", req)
		s.Close()
		if e != nil {
			t.Fatal(e)
		}
		var env struct {
			OK     bool
			Result interceptResponse
		}
		json.Unmarshal(got, &env)
		if !env.OK || len(env.Result.Body) != 0 {
			t.Fatalf("not fail-open: %s", got)
		}
	}
}
func TestNoToolOutputNoNetwork(t *testing.T) {
	setupConfig(t, "http://127.0.0.1:1", "compress_user_messages: false\n")
	raw := []byte(`{"messages":[{"role":"user","content":"long user request remains intact"}]}`)
	got, e := compressBody(interceptRequest{Body: raw}, settings.Load())
	if e != nil || got != nil {
		t.Fatal(e, string(got))
	}
}
func TestUserMessagesDefaultOnAndRuntimeOption(t *testing.T) {
	long := strings.Repeat("document paragraph ", 20)
	cases := []string{
		`{"messages":[{"role":"system","content":"INSTRUCTIONS"},{"role":"user","content":"LONG"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"LONG"}]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_text","text":"LONG"}]}]}`,
		`{"contents":[{"role":"user","parts":[{"text":"LONG"}]}]}`,
	}
	seen := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
			return
		}
		cfg := b["config"].(map[string]any)
		if cfg["compress_user_messages"] != true {
			t.Error("runtime user option is not enabled")
		}
		msgs := b["messages"].([]any)
		if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
			t.Error("only user text should be sent", msgs)
		}
		msgs[0].(map[string]any)["content"] = "short"
		seen++
		json.NewEncoder(w).Encode(map[string]any{"messages": msgs, "ccr_hashes": []string{}})
	}))
	defer s.Close()
	setup(t, s.URL)
	for _, c := range cases {
		raw := []byte(strings.ReplaceAll(c, "LONG", long))
		got, err := compressBody(interceptRequest{Body: raw, Model: "x"}, settings.Load())
		if err != nil || !bytes.Contains(got, []byte("short")) || bytes.Contains(got, []byte(long)) {
			t.Fatalf("user text not compressed: %v %s", err, got)
		}
		if bytes.Contains(raw, []byte("INSTRUCTIONS")) && !bytes.Contains(got, []byte("INSTRUCTIONS")) {
			t.Fatal("system message changed")
		}
	}
	if seen != len(cases) {
		t.Fatalf("Headroom calls = %d", seen)
	}
}
func TestUserCompressionOffStillCompressesTool(t *testing.T) {
	long := strings.Repeat("document paragraph ", 20)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		if b["config"].(map[string]any)["compress_user_messages"] != false {
			t.Error("runtime option should be false")
		}
		msgs := b["messages"].([]any)
		if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "tool" {
			t.Error("wrong candidate selection", msgs)
		}
		msgs[0].(map[string]any)["content"] = "short"
		json.NewEncoder(w).Encode(map[string]any{"messages": msgs})
	}))
	defer s.Close()
	setupConfig(t, s.URL, "compress_user_messages: false\n")
	raw := []byte(`{"messages":[{"role":"user","content":"LONG"},{"role":"tool","content":"LONG"}]}`)
	raw = []byte(strings.ReplaceAll(string(raw), "LONG", long))
	got, err := compressBody(interceptRequest{Body: raw}, settings.Load())
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	json.Unmarshal(got, &body)
	msgs := body["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != long || msgs[1].(map[string]any)["content"] != "short" {
		t.Fatal("toggle was not respected")
	}
}
func TestTimeout(t *testing.T) {
	done := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-done }))
	defer s.Close()
	defer close(done)
	setup(t, s.URL)
	settings.Load().client.Timeout = 10 * time.Millisecond
	got, e := compressBody(interceptRequest{Body: []byte(`{"messages":[{"role":"tool","content":"long enough to compress"}]}`)}, settings.Load())
	if e == nil || got != nil {
		t.Fatal("timeout must preserve original")
	}
}
func TestLiveHeadroom(t *testing.T) {
	u := os.Getenv("HEADROOM_TEST_URL")
	if u == "" {
		t.Skip("set HEADROOM_TEST_URL for real compression")
	}
	setup(t, u)
	settings.Load().client.Timeout = 60 * time.Second
	var log strings.Builder
	for i := 0; i < 500; i++ {
		log.WriteString("2026-09-16 INFO health check succeeded status=200 service=api latency_ms=2\n")
	}
	log.WriteString("2026-09-16 ERROR payment failed transaction=TX-123 reason=insufficient_funds\n")
	raw, _ := json.Marshal(map[string]any{"model": "gpt-4o", "messages": []any{map[string]any{"role": "tool", "tool_call_id": "real-id", "content": log.String()}}})
	got, e := compressBody(interceptRequest{Body: raw, Model: "gpt-4o", SourceFormat: "openai"}, settings.Load())
	if e != nil {
		t.Fatal(e)
	}
	if len(got) == 0 || len(got) >= len(raw) {
		t.Fatal("no compression")
	}
	if !bytes.Contains(got, []byte("TX-123")) {
		t.Fatal("lost unique error")
	}
	t.Logf("real Headroom: %d -> %d bytes", len(raw), len(got))
}
func TestDispatchRecoversPanic(t *testing.T) {
	raw, ok := dispatch("request.intercept_before", nil, func(string, []byte) ([]byte, error) { panic("boom") })
	var env envelope
	if ok || json.Unmarshal(raw, &env) != nil || env.OK || env.Error == nil || env.Error.Code != "plugin_panic" || env.Error.Message != "boom" {
		t.Fatalf("ok=%v raw=%s", ok, raw)
	}
	raw, ok = dispatch("x", nil, func(string, []byte) ([]byte, error) { return nil, os.ErrNotExist })
	if ok || json.Unmarshal(raw, &env) != nil || env.Error == nil || env.Error.Code != "plugin_error" {
		t.Fatalf("ok=%v raw=%s", ok, raw)
	}
}
func TestDefaultEndpointIsLoopback(t *testing.T) {
	r, _ := json.Marshal(map[string]any{"config_yaml": []byte("min_chars: 10\n")})
	if e := configure(r); e != nil {
		t.Fatal(e)
	}
	if got := settings.Load().Endpoint; got != "http://127.0.0.1:8787/v1/compress" {
		t.Fatalf("default endpoint = %s", got)
	}
}
