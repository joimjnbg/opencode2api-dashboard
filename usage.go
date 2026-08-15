package main

import (
	"bufio"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

// usageFromResponse extracts token usage from an upstream JSON response body
// according to the protocol that produced it.
func usageFromResponse(protocol Protocol, body []byte) bridgeUsage {
	var value map[string]any
	if json.Unmarshal(body, &value) != nil {
		return bridgeUsage{}
	}
	switch protocol {
	case ProtocolAnthropic:
		return decodeAnthropicUsage(mapAt(value, "usage"))
	default:
		return decodeOpenAIUsage(mapAt(value, "usage"))
	}
}

// costFromResponse extracts a monetary cost field when the upstream reports
// one (OpenAI-style chat completions carry "cost" as a string or number).
func costFromResponse(body []byte) float64 {
	var value map[string]any
	if json.Unmarshal(body, &value) != nil {
		return 0
	}
	raw, exists := value["cost"]
	if !exists {
		return 0
	}
	switch cost := raw.(type) {
	case float64:
		return cost
	case string:
		parsed, _ := strconv.ParseFloat(strings.TrimSpace(cost), 64)
		return parsed
	default:
		return 0
	}
}

// usageExtractReader passes through an SSE stream byte-for-byte while scanning
// data events for usage blocks. Every event that carries token usage is
// recorded into the gateway stats. Streamed responses are never rewritten.
type usageExtractReader struct {
	reader   io.Reader
	scanner  *bufio.Scanner
	protocol Protocol
	model    string
	stats    *usageStats
	pending  []byte
	done     bool
}

func newUsageExtractReader(r io.Reader, protocol Protocol, model string, stats *usageStats) *usageExtractReader {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	return &usageExtractReader{
		reader:   r,
		scanner:  scanner,
		protocol: protocol,
		model:    model,
		stats:    stats,
	}
}

func (u *usageExtractReader) Read(p []byte) (int, error) {
	if len(u.pending) == 0 && !u.done {
		if !u.scanner.Scan() {
			u.done = true
			if err := u.scanner.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		line := u.scanner.Text()
		u.processLine(line)
		u.pending = append(u.pending, line...)
		u.pending = append(u.pending, '\n')
	}
	if len(u.pending) > 0 {
		n := copy(p, u.pending)
		u.pending = u.pending[n:]
		return n, nil
	}
	if u.done {
		return 0, io.EOF
	}
	return 0, nil
}

// processLine inspects a single SSE line (already stripped of \n). It looks
// for "data:" payloads that contain token usage and records them.
func (u *usageExtractReader) processLine(line string) {
	line = strings.TrimSuffix(line, "\r")
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	var event map[string]any
	if json.Unmarshal([]byte(payload), &event) != nil {
		return
	}
	usage, ok := extractStreamUsage(u.protocol, event)
	if !ok {
		return
	}
	cost := 0.0
	if raw, exists := event["cost"]; exists {
		switch c := raw.(type) {
		case float64:
			cost = c
		case string:
			if parsed, err := strconv.ParseFloat(strings.TrimSpace(c), 64); err == nil {
				cost = parsed
			}
		}
	}
	u.stats.Record(u.model, usage, cost, true)
}

// extractStreamUsage returns usage from a single SSE event object when it
// carries token accounting for the given protocol.
func extractStreamUsage(protocol Protocol, event map[string]any) (bridgeUsage, bool) {
	switch protocol {
	case ProtocolAnthropic:
		// message_delta carries final usage.
		if stringAt(event, "type") == "message_delta" {
			if usage := mapAt(event, "usage"); len(usage) > 0 {
				return decodeAnthropicUsage(usage), true
			}
		}
	case ProtocolResponses:
		// response.completed / response.incomplete / response.failed carry usage.
		response := mapAt(event, "response")
		if usage := mapAt(response, "usage"); len(usage) > 0 {
			return decodeOpenAIUsage(usage), true
		}
	default:
		// Chat completions attach usage to the final chunk (choices is empty).
		if usage := mapAt(event, "usage"); len(usage) > 0 {
			return decodeOpenAIUsage(usage), true
		}
	}
	return bridgeUsage{}, false
}
