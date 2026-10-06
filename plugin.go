package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"time"
)

const version = "0.6.0"

var repository = "https://github.com/frankyw/cliproxyapi-headroom"
var author = "frankyw"

type config struct {
	ServiceURL           string  `yaml:"service_url"`
	StatsPath            string  `yaml:"stats_path"`
	Endpoint             string  `yaml:"endpoint"`
	TokenEnv             string  `yaml:"token_env"`
	TimeoutMS            int     `yaml:"timeout_ms"`
	MinChars             int     `yaml:"min_chars"`
	TargetRatio          float64 `yaml:"target_ratio"`
	Mode                 string  `yaml:"mode"`
	CompressUserMessages bool    `yaml:"compress_user_messages"`
}
type runtimeConfig struct {
	config
	client  *http.Client
	stats   *statsStore
	service *serviceCache
}

var settings atomic.Pointer[runtimeConfig]

type interceptRequest struct {
	Body         []byte
	Model        string
	SourceFormat string
}
type interceptResponse struct{ Body []byte }

func configure(raw []byte) error {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return err
	}
	cfg := config{Endpoint: "http://127.0.0.1:8787/v1/compress", TimeoutMS: 10000, MinChars: 512, TargetRatio: 0.5, CompressUserMessages: true}
	if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
		return err
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return errors.New("endpoint must be an HTTP(S) URL without embedded credentials")
	}
	if cfg.TimeoutMS < 1 || cfg.TimeoutMS > 120000 || cfg.MinChars < 1 || cfg.TargetRatio <= 0 || cfg.TargetRatio >= 1 || (cfg.Mode != "" && cfg.Mode != "lossy_inline") {
		return errors.New("invalid timeout_ms, min_chars, target_ratio or mode")
	}
	if cfg.ServiceURL != "" {
		serviceURL, err := url.Parse(cfg.ServiceURL)
		if err != nil || serviceURL.Host == "" || (serviceURL.Scheme != "http" && serviceURL.Scheme != "https") || serviceURL.User != nil {
			return errors.New("service_url must be HTTP(S) without embedded credentials")
		}
		cfg.ServiceURL = strings.TrimRight(cfg.ServiceURL, "/")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	store, err := configureStats(cfg.StatsPath)
	if err != nil {
		return err
	}
	settings.Store(&runtimeConfig{cfg, &http.Client{Timeout: time.Duration(cfg.TimeoutMS) * time.Millisecond, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, store, &serviceCache{}})
	return nil
}
func handleMethod(method string, raw []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if err := configure(raw); err != nil {
			return nil, err
		}
		fields := []map[string]any{}
		for _, f := range []struct{ name, kind, desc string }{{"service_url", "string", "Optional Headroom service root for health and statistics; defaults to endpoint origin"}, {"stats_path", "string", "Persistent statistics file; empty means memory only"}, {"endpoint", "string", "Headroom compression-only URL"}, {"token_env", "string", "Optional environment variable containing the Headroom token"}, {"timeout_ms", "integer", "Compression timeout; original request is used on failure"}, {"min_chars", "integer", "Minimum text characters to compress"}, {"target_ratio", "number", "Requested retained content ratio"}, {"mode", "string", "Empty follows Headroom server mode; lossy_inline can drop words from text"}, {"compress_user_messages", "boolean", "Compress eligible user-message text; defaults to true. System messages remain unchanged"}} {
			fields = append(fields, map[string]any{"Name": f.name, "Type": f.kind, "Description": f.desc})
		}
		return okEnvelope(map[string]any{"schema_version": 6, "metadata": map[string]any{"Name": "headroom", "Version": version, "Author": author, "GitHubRepository": repository, "ConfigFields": fields}, "capabilities": map[string]any{"request_interceptor": true, "management_api": true}})
	case "request.intercept_before":
		var req interceptRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		body, err := compressBody(req, settings.Load())
		if err != nil {
			fmt.Fprintln(os.Stderr, "headroom: compression skipped:", err)
			body = nil
		}
		return okEnvelope(interceptResponse{Body: body})
	case "management.register":
		return managementRegistration()
	case "management.handle":
		return managementHandle(raw)
	case "plugin.quiesce", "plugin.shutdown":
		flushStats()
		return okEnvelope(struct{}{})
	case "request.intercept_after":
		return okEnvelope(struct{}{})
	default:
		return errorEnvelope("unknown_method", "unsupported method"), nil
	}
}

// compressionSlots caps concurrent interceptions so that many large requests
// held by a slow Headroom cannot keep unbounded decoded copies in memory.
// Acquisition never blocks: when saturated the request passes unchanged.
var compressionSlots = make(chan struct{}, 8)

func acquireCompressionSlot() bool {
	select {
	case compressionSlots <- struct{}{}:
		return true
	default:
		return false
	}
}
func releaseCompressionSlot() { <-compressionSlots }

func busyResponse() []byte {
	if cfg := settings.Load(); cfg != nil && cfg.stats != nil {
		cfg.stats.record(metricEvent{Status: "fallback", Reason: "compression concurrency limit reached"})
	}
	raw, _ := okEnvelope(interceptResponse{})
	return raw
}

type candidate struct {
	text  string
	role  string
	apply func(string)
}

// Project eligible text while preserving the original protocol and IDs.
func candidates(body map[string]any, min int, includeUser bool) []candidate {
	var out []candidate
	add := func(m map[string]any, key, role string) {
		if s, ok := m[key].(string); ok && len(s) >= min {
			out = append(out, candidate{s, role, func(v string) { m[key] = v }})
		}
	}
	textBlocks := func(m map[string]any, key, role string) {
		add(m, key, role)
		if blocks, ok := m[key].([]any); ok {
			for _, b := range blocks {
				if block, ok := b.(map[string]any); ok {
					t, _ := block["type"].(string)
					if t == "text" || t == "input_text" || t == "output_text" {
						add(block, "text", role)
					}
				}
			}
		}
	}
	if msgs, ok := body["messages"].([]any); ok {
		for _, item := range msgs {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if m["role"] == "tool" || m["role"] == "function" {
				textBlocks(m, "content", "tool")
			}
			if includeUser && m["role"] == "user" {
				textBlocks(m, "content", "user")
			}
			if blocks, ok := m["content"].([]any); ok {
				for _, b := range blocks {
					if block, ok := b.(map[string]any); ok && block["type"] == "tool_result" {
						textBlocks(block, "content", "tool")
					}
				}
			}
		}
	}
	if input, ok := body["input"].([]any); ok {
		for _, item := range input {
			if m, ok := item.(map[string]any); ok {
				if m["type"] == "function_call_output" {
					textBlocks(m, "output", "tool")
				}
				if includeUser && m["role"] == "user" {
					textBlocks(m, "content", "user")
				}
			}
		}
	}
	// Gemini function responses stay objects; only string leaves are eligible.
	var leaves func(any)
	leaves = func(v any) {
		switch m := v.(type) {
		case map[string]any:
			for k, v := range m {
				if _, ok := v.(string); ok {
					add(m, k, "tool")
				} else {
					leaves(v)
				}
			}
		case []any:
			for _, v := range m {
				leaves(v)
			}
		}
	}
	if contents, ok := body["contents"].([]any); ok {
		for _, item := range contents {
			if m, ok := item.(map[string]any); ok {
				if parts, ok := m["parts"].([]any); ok {
					for _, p := range parts {
						if part, ok := p.(map[string]any); ok {
							if f, ok := part["functionResponse"].(map[string]any); ok {
								leaves(f["response"])
							}
							if includeUser && m["role"] == "user" {
								add(part, "text", "user")
							}
						}
					}
				}
			}
		}
	}
	return out
}
func compressBody(req interceptRequest, cfg *runtimeConfig) (output []byte, failure error) {
	started := time.Now()
	event := metricEvent{Model: req.Model, Status: "skipped"}
	defer func() {
		if cfg == nil || cfg.stats == nil {
			return
		}
		event.LatencyMS = float64(time.Since(started).Microseconds()) / 1000
		if failure != nil {
			event.Status = "fallback"
			event.Reason = failure.Error()
		}
		cfg.stats.record(event)
	}()
	if cfg == nil {
		return nil, errors.New("configuration unavailable")
	}
	if len(req.Body) > 32<<20 {
		return nil, errors.New("request exceeds compression size limit")
	}
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(req.Body))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		return nil, nil
	}
	// Re-encoding only the first value would silently drop trailing data.
	if _, err := dec.Token(); err != io.EOF {
		return nil, nil
	}
	selected := candidates(body, cfg.MinChars, cfg.CompressUserMessages)
	if len(selected) == 0 {
		return nil, nil
	}
	messages := make([]map[string]any, 0, len(selected))
	for i, c := range selected {
		message := map[string]any{"role": c.role, "content": c.text}
		if c.role == "tool" {
			message["tool_call_id"] = fmt.Sprintf("headroom_%d", i)
		}
		messages = append(messages, message)
	}
	compressionConfig := map[string]any{"target_ratio": cfg.TargetRatio, "protect_recent": 0, "protect_analysis_context": false, "compress_user_messages": cfg.CompressUserMessages}
	if cfg.Mode != "" {
		compressionConfig["mode"] = cfg.Mode
	}
	payload, _ := json.Marshal(map[string]any{"model": req.Model, "messages": messages, "config": compressionConfig})
	request, err := http.NewRequest(http.MethodPost, cfg.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("invalid compression request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "cliproxyapi-headroom/"+version)
	if cfg.TokenEnv != "" {
		token := os.Getenv(cfg.TokenEnv)
		if token == "" {
			return nil, errors.New("configured token environment variable is missing")
		}
		request.Header.Set("Authorization", "Bearer "+token)
	}
	event.Attempt = true
	event.Status = "unchanged"
	res, err := cfg.client.Do(request)
	if err != nil {
		return nil, errors.New("Headroom unavailable or timed out")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("Headroom HTTP %d", res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, (32<<20)+1))
	if err != nil || len(raw) > 32<<20 {
		return nil, errors.New("invalid or oversized Headroom response")
	}
	var result struct {
		Messages []map[string]any `json:"messages"`
		Hashes   []any            `json:"ccr_hashes"`
		Skipped  bool             `json:"compression_skipped"`
		Before   int              `json:"tokens_before"`
		After    int              `json:"tokens_after"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, errors.New("invalid Headroom JSON")
	}
	if result.Skipped {
		return nil, errors.New("Headroom skipped compression")
	}
	if len(result.Hashes) > 0 || len(result.Messages) != len(messages) {
		return nil, errors.New("Headroom returned retrieval markers or changed message count")
	}
	changed := 0
	allAccepted := true
	var beforeBytes, afterBytes int64
	for i, m := range result.Messages {
		s, ok := m["content"].(string)
		original := messages[i]
		copyM := make(map[string]any, len(m))
		for k, v := range m {
			copyM[k] = v
		}
		copyM["content"] = original["content"]
		if !ok || !reflect.DeepEqual(copyM, original) || strings.TrimSpace(s) == "" || strings.Contains(s, "[headroom:") {
			return nil, errors.New("Headroom changed tool structure or returned invalid content")
		}
		beforeBytes += int64(len(selected[i].text))
		afterBytes += int64(len(selected[i].text))
		if len(s) > len(selected[i].text) {
			allAccepted = false
		}
		if len(s) < len(selected[i].text) {
			afterBytes -= int64(len(selected[i].text) - len(s))
			selected[i].apply(s)
			changed++
		}
	}
	if changed == 0 {
		return nil, nil
	}
	compressed, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "headroom: compressed format=%s outputs=%d tokens_before=%d tokens_after=%d bytes_before=%d bytes_after=%d\n", req.SourceFormat, changed, result.Before, result.After, len(req.Body), len(compressed))
	event.Status = "compressed"
	event.BytesBefore = beforeBytes
	event.BytesAfter = afterBytes
	if allAccepted && result.Before > 0 && result.After >= 0 && result.After <= result.Before {
		event.TokenKnown = true
		event.TokensBefore = int64(result.Before)
		event.TokensAfter = int64(result.After)
	}
	return compressed, nil
}
