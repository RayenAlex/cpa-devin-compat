package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func call(t *testing.T, method string, request any, result any) {
	t.Helper()
	raw, _ := json.Marshal(request)
	out, err := handleMethod(method, raw)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil || !envelope.OK {
		t.Fatalf("%s: bad envelope %s", method, out)
	}
	if result != nil {
		if err := json.Unmarshal(envelope.Result, result); err != nil {
			t.Fatalf("%s: decode result: %v", method, err)
		}
	}
}

func resetConfig() {
	c := defaultConfig()
	c.Log = false
	settings.Store(&c)
}

func TestRegisterDeclaresInterceptors(t *testing.T) {
	var result struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	call(t, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte("log: false\n")}, &result)
	for _, name := range []string{"request_interceptor", "response_interceptor", "response_stream_interceptor"} {
		if !result.Capabilities[name] {
			t.Fatalf("capability %s not declared: %v", name, result.Capabilities)
		}
	}
	resetConfig()
}

func chatBody(extra ...map[string]any) []byte {
	messages := []map[string]any{
		{"role": "system", "content": "You are opencode."},
		{"role": "user", "content": "fix the bug"},
	}
	messages = append(messages, extra...)
	b, _ := json.Marshal(map[string]any{"model": "devin/swe-2", "messages": messages})
	return b
}

func TestSessionPinOnlyWithoutExplicitSession(t *testing.T) {
	resetConfig()
	req := pluginapi.RequestInterceptRequest{SourceFormat: "openai", RequestedModel: "devin/swe-2", Body: chatBody()}
	var first pluginapi.RequestInterceptResponse
	call(t, pluginabi.MethodRequestInterceptBefore, req, &first)
	id := first.Headers.Get("X-Session-Id")
	if !strings.HasPrefix(id, pinnedSessionIDPrefix) {
		t.Fatalf("expected pinned session header, got %v", first.Headers)
	}
	if len(first.Body) != 0 {
		t.Fatalf("chat body must stay untouched")
	}

	// 后续轮次与历史被改写的分叉：系统提示词和首条用户消息不变，ID 必须不变。
	req.Body = chatBody(
		map[string]any{"role": "assistant", "content": "done"},
		map[string]any{"role": "user", "content": "next"},
	)
	var later pluginapi.RequestInterceptResponse
	call(t, pluginabi.MethodRequestInterceptBefore, req, &later)
	if got := later.Headers.Get("X-Session-Id"); got != id {
		t.Fatalf("session id changed across turns: %s vs %s", got, id)
	}

	req.Headers = http.Header{"X-Session-Affinity": {"ses_client"}}
	var explicit pluginapi.RequestInterceptResponse
	call(t, pluginabi.MethodRequestInterceptBefore, req, &explicit)
	if explicit.Headers.Get("X-Session-Id") != "" {
		t.Fatalf("must not override explicit client session: %v", explicit.Headers)
	}

	req.Headers = nil
	req.RequestedModel = "gpt-5.6-terra"
	var other pluginapi.RequestInterceptResponse
	call(t, pluginabi.MethodRequestInterceptBefore, req, &other)
	if len(other.Headers) != 0 {
		t.Fatalf("non-devin model must be ignored")
	}
}

func TestFlattenNamespaceTools(t *testing.T) {
	body := []byte(`{"model":"devin/swe-2","tools":[` +
		`{"type":"function","name":"shell","parameters":{"type":"object"}},` +
		`{"type":"namespace","name":"mcp__docs","description":"docs","tools":[` +
		`{"type":"function","name":"search_docs","description":"search","parameters":{"type":"object","properties":{"q":{"type":"string"}}}},` +
		`{"type":"function","name":"shell","parameters":{}}]}],"input":"hi"}`)
	out, namespaces, tools := flattenNamespaceTools(body)
	if namespaces != 1 || tools != 1 {
		t.Fatalf("namespaces=%d tools=%d", namespaces, tools)
	}
	names := []string{}
	for _, tool := range gjson.GetBytes(out, "tools").Array() {
		if tool.Get("type").String() != "function" {
			t.Fatalf("non-function tool left: %s", tool.Raw)
		}
		names = append(names, tool.Get("name").String())
	}
	if strings.Join(names, ",") != "shell,search_docs" {
		t.Fatalf("unexpected tools %v", names)
	}
	if gjson.GetBytes(out, "input").String() != "hi" {
		t.Fatalf("other fields must be preserved")
	}
	if _, n, _ := flattenNamespaceTools([]byte(`{"tools":[{"type":"function","name":"a"}]}`)); n != 0 {
		t.Fatalf("plain tools must be untouched")
	}
}

func TestFlattenNamespaceToolsFromCodexAdditionalTools(t *testing.T) {
	body := []byte(`{
		"model":"devin/swe-2",
		"input":[
			{"role":"user","content":"fix it"},
			{"type":"additional_tools","tools":[
				{"type":"function","name":"shell","parameters":{"type":"object"}},
				{"type":"namespace","name":"functions","tools":[
					{"type":"custom","name":"apply_patch","description":"apply patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},
					{"type":"function","name":"wait","parameters":{"type":"object"}},
					{"type":"function","name":"shell","parameters":{"type":"object"}}
				]}
			]}
		]
	}`)

	out, namespaces, tools := flattenNamespaceTools(body)
	if namespaces != 1 || tools != 2 {
		t.Fatalf("namespaces=%d tools=%d", namespaces, tools)
	}

	additional := gjson.GetBytes(out, "input.1.tools").Array()
	if len(additional) != 3 {
		t.Fatalf("additional tools len=%d: %s", len(additional), out)
	}
	if got := additional[0].Get("name").String(); got != "shell" {
		t.Fatalf("first tool=%q", got)
	}
	if got, typ := additional[1].Get("name").String(), additional[1].Get("type").String(); got != "apply_patch" || typ != "custom" {
		t.Fatalf("custom tool not preserved: %s", additional[1].Raw)
	}
	if got := additional[2].Get("name").String(); got != "wait" {
		t.Fatalf("third tool=%q", got)
	}
	if gjson.GetBytes(out, "input.0.content").String() != "fix it" {
		t.Fatalf("ordinary input item changed: %s", out)
	}
	if gjson.GetBytes(out, "input.1.tools.#(type==\"namespace\")").Exists() {
		t.Fatalf("namespace tool remained: %s", out)
	}
}

func TestFlattenNamespaceToolsDeduplicatesAcrossResponsesToolLocations(t *testing.T) {
	body := []byte(`{
		"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],
		"input":[{"type":"additional_tools","tools":[
			{"type":"namespace","name":"functions","tools":[
				{"type":"function","name":"shell","parameters":{"type":"object"}},
				{"type":"function","name":"wait","parameters":{"type":"object"}}
			]}
		]}]
	}`)

	out, namespaces, tools := flattenNamespaceTools(body)
	if namespaces != 1 || tools != 1 {
		t.Fatalf("namespaces=%d tools=%d", namespaces, tools)
	}
	if got := gjson.GetBytes(out, "input.0.tools.0.name").String(); got != "wait" {
		t.Fatalf("cross-location duplicate not removed, got=%q body=%s", got, out)
	}
}

func TestRequestInterceptorFlattensCodexResponsesLiteTools(t *testing.T) {
	resetConfig()
	req := pluginapi.RequestInterceptRequest{
		SourceFormat:   "openai-response",
		RequestedModel: "devin/swe-2",
		Body:           []byte(`{"model":"devin/swe-2","input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"exec","parameters":{"type":"object"}}]}]}]}`),
	}
	var resp pluginapi.RequestInterceptResponse
	call(t, pluginabi.MethodRequestInterceptBefore, req, &resp)
	if len(resp.Body) == 0 {
		t.Fatal("Codex Responses Lite additional_tools request was not rewritten")
	}
	if got := gjson.GetBytes(resp.Body, "input.0.tools.0.name").String(); got != "exec" {
		t.Fatalf("flattened tool=%q body=%s", got, resp.Body)
	}
}

func TestSanitizeResponsesToolSchemasInlinesLocalRefs(t *testing.T) {
	body := []byte(`{
		"tools":[{
			"type":"function",
			"name":"automation_update",
			"parameters":{
				"$defs":{
					"string":{"type":"string"},
					"nullable":{"anyOf":[{"$ref":"#/$defs/string"},{"type":"null"}]},
					"cron":{"type":"object","properties":{
						"name":{"$ref":"#/$defs/string"},
						"notificationPolicy":{"$ref":"#/$defs/nullable"}
					}}
				},
				"oneOf":[{"$ref":"#/$defs/cron"}],
				"properties":{},
				"type":"object"
			}
		}],
		"input":[{"type":"additional_tools","tools":[{
			"type":"function",
			"name":"secondary",
			"parameters":{"definitions":{"value":{"type":"integer"}},"properties":{"count":{"$ref":"#/definitions/value"}},"type":"object"}
		}]}]
	}`)

	out, schemas, refs := sanitizeResponsesToolSchemas(body)
	if schemas != 2 || refs != 5 {
		t.Fatalf("schemas=%d refs=%d body=%s", schemas, refs, out)
	}
	if gjson.GetBytes(out, `tools.0.parameters.$defs`).Exists() ||
		gjson.GetBytes(out, `input.0.tools.0.parameters.definitions`).Exists() {
		t.Fatalf("schema definitions were not removed: %s", out)
	}
	if strings.Contains(string(out), `"$ref"`) {
		t.Fatalf("local refs remained: %s", out)
	}
	if got := gjson.GetBytes(out, "tools.0.parameters.oneOf.0.properties.name.type").String(); got != "string" {
		t.Fatalf("inlined name type=%q body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "tools.0.parameters.oneOf.0.properties.notificationPolicy.anyOf.0.type").String(); got != "string" {
		t.Fatalf("nested inlined type=%q body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "input.0.tools.0.parameters.properties.count.type").String(); got != "integer" {
		t.Fatalf("definitions ref type=%q body=%s", got, out)
	}
}

func TestSanitizeResponsesToolSchemasBreaksRecursiveLocalRefs(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","name":"tree","parameters":{
		"$defs":{"node":{"type":"object","properties":{"child":{"$ref":"#/$defs/node"}}}},
		"type":"object","properties":{"root":{"$ref":"#/$defs/node"}}
	}}]}`)

	out, schemas, refs := sanitizeResponsesToolSchemas(body)
	if schemas != 1 || refs != 2 {
		t.Fatalf("schemas=%d refs=%d body=%s", schemas, refs, out)
	}
	if strings.Contains(string(out), `"$ref"`) || strings.Contains(string(out), `"$defs"`) {
		t.Fatalf("recursive ref remained: %s", out)
	}
	if got := gjson.GetBytes(out, "tools.0.parameters.properties.root.type").String(); got != "object" {
		t.Fatalf("root schema lost: %s", out)
	}
	if raw := gjson.GetBytes(out, "tools.0.parameters.properties.root.properties.child").Raw; raw != `{}` {
		t.Fatalf("recursive edge must become permissive schema, got=%s body=%s", raw, out)
	}
}

func TestRequestInterceptorSanitizesResponsesToolSchemas(t *testing.T) {
	resetConfig()
	req := pluginapi.RequestInterceptRequest{
		SourceFormat:   "openai-response",
		RequestedModel: "devin/swe-2",
		Body: []byte(`{"model":"devin/swe-2","tools":[{"type":"function","name":"automation_update","parameters":{
			"$defs":{"s":{"type":"string"}},"type":"object","properties":{"id":{"$ref":"#/$defs/s"}}
		}}],"input":"hi"}`),
	}
	var resp pluginapi.RequestInterceptResponse
	call(t, pluginabi.MethodRequestInterceptBefore, req, &resp)
	if len(resp.Body) == 0 {
		t.Fatal("Responses tool schema was not rewritten")
	}
	if got := gjson.GetBytes(resp.Body, "tools.0.parameters.properties.id.type").String(); got != "string" {
		t.Fatalf("schema was not inlined, got=%q body=%s", got, resp.Body)
	}
	if strings.Contains(string(resp.Body), `"$ref"`) || strings.Contains(string(resp.Body), `"$defs"`) {
		t.Fatalf("unsupported local schema references remained: %s", resp.Body)
	}
}

func sse(event, data string) []byte {
	return []byte("event: " + event + "\ndata: " + data)
}

func chunk(t *testing.T, requestID string, body []byte) gjson.Result {
	t.Helper()
	var resp pluginapi.StreamChunkInterceptResponse
	call(t, pluginabi.MethodResponseInterceptStreamChunk, pluginapi.StreamChunkInterceptRequest{
		RequestID: requestID, SourceFormat: "openai-response", RequestedModel: "devin/swe-2", Body: body, ChunkIndex: 1,
	}, &resp)
	out := body
	if len(resp.Body) > 0 {
		out = resp.Body
	}
	if !strings.HasPrefix(string(out), "event: ") {
		t.Fatalf("SSE framing lost: %q", out)
	}
	return gjson.Parse(string(out[strings.Index(string(out), "{"):]))
}

func TestPatchResponsesStream(t *testing.T) {
	resetConfig()
	const rid = "req-1"
	created := chunk(t, rid, sse("response.created", `{"type":"response.created","response":{"id":"interaction_x","object":"response","status":"in_progress","model":"devin/swe-2","output":[]},"sequence_number":1}`))
	if !created.Get("response.created_at").Exists() {
		t.Fatalf("created_at missing: %s", created.Raw)
	}
	chunk(t, rid, sse("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"item_0","type":"reasoning","status":"in_progress","encrypted_content":"","summary":[]},"sequence_number":2}`))
	delta := chunk(t, rid, sse("response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"thinking","sequence_number":3}`))
	if delta.Get("item_id").String() != "item_0" || !delta.Get("summary_index").Exists() {
		t.Fatalf("reasoning delta not patched: %s", delta.Raw)
	}
	added := chunk(t, rid, sse("response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"id":"bash_0#a","type":"function_call","call_id":"bash_0#a","name":"bash","arguments":""},"sequence_number":4}`))
	if added.Get("item.status").String() != "in_progress" {
		t.Fatalf("added status: %s", added.Raw)
	}
	done := chunk(t, rid, sse("response.output_item.done", `{"type":"response.output_item.done","output_index":1,"item":{"id":"bash_0#a","type":"function_call","call_id":"bash_0#a","name":"bash","arguments":"{}"},"sequence_number":5}`))
	if done.Get("item.status").String() != "completed" {
		t.Fatalf("done status: %s", done.Raw)
	}
	completed := chunk(t, rid, sse("response.completed", `{"type":"response.completed","response":{"id":"interaction_x","object":"response","status":"completed","model":"devin/swe-2","output":[{"type":"function_call","call_id":"bash_0#a","name":"bash","arguments":"{}"}],"usage":{}},"sequence_number":6}`))
	if !completed.Get("response.created_at").Exists() || completed.Get("response.output.0.id").String() != "bash_0#a" {
		t.Fatalf("completed not patched: %s", completed.Raw)
	}
	statesMu.Lock()
	_, left := states[rid]
	statesMu.Unlock()
	if left {
		t.Fatalf("state must be dropped after terminal event")
	}

	// 已经合规的事件原样放行（不回写 Body）。
	var resp pluginapi.StreamChunkInterceptResponse
	call(t, pluginabi.MethodResponseInterceptStreamChunk, pluginapi.StreamChunkInterceptRequest{
		RequestID: "req-2", SourceFormat: "openai-response", RequestedModel: "devin/swe-2", ChunkIndex: 0,
		Body: sse("response.output_text.delta", `{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"item_0","delta":"hi"}`),
	}, &resp)
	if len(resp.Body) != 0 {
		t.Fatalf("compliant chunk must pass through")
	}
}

func TestPatchResponsesNonStream(t *testing.T) {
	resetConfig()
	body := []byte(`{"id":"interaction_y","object":"response","status":"completed","model":"devin/swe-2","output":[` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"t"}]},` +
		`{"type":"function_call","call_id":"read_0#b","name":"read","arguments":"{}"},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
	var resp pluginapi.ResponseInterceptResponse
	call(t, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{
		SourceFormat: "openai-response", RequestedModel: "devin/swe-2", StatusCode: 200, Body: body,
	}, &resp)
	out := gjson.ParseBytes(resp.Body)
	if !out.Get("created_at").Exists() || out.Get("output.0.id").String() == "" ||
		out.Get("output.1.id").String() != "read_0#b" || out.Get("output.1.status").String() != "completed" ||
		out.Get("output.2.id").String() == "" || !out.Get("output.2.content.0.annotations").IsArray() {
		t.Fatalf("non-stream not patched: %s", resp.Body)
	}
}

func TestPatchResponsesCustomToolItems(t *testing.T) {
	resetConfig()
	added := chunk(t, "custom-1", sse("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":""},"sequence_number":1}`))
	if added.Get("item.id").String() != "call_patch" || added.Get("item.status").String() != "in_progress" {
		t.Fatalf("custom added item not patched: %s", added.Raw)
	}

	done := chunk(t, "custom-1", sse("response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":"*** Begin Patch"},"sequence_number":2}`))
	if done.Get("item.id").String() != "call_patch" || done.Get("item.status").String() != "completed" {
		t.Fatalf("custom done item not patched: %s", done.Raw)
	}

	out, fixes := patchResponsesObject([]byte(`{"id":"response_custom","object":"response","status":"completed","output":[{"type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":"patch"}]}`))
	if fixes == 0 {
		t.Fatal("non-stream custom tool item was not patched")
	}
	root := gjson.ParseBytes(out)
	if root.Get("output.0.id").String() != "call_patch" || root.Get("output.0.status").String() != "completed" {
		t.Fatalf("non-stream custom tool item not patched: %s", out)
	}
}

func TestNormalizeChatToolIndex(t *testing.T) {
	resetConfig()
	send := func(body string) gjson.Result {
		var resp pluginapi.StreamChunkInterceptResponse
		call(t, pluginabi.MethodResponseInterceptStreamChunk, pluginapi.StreamChunkInterceptRequest{
			RequestID: "chat-1", SourceFormat: "openai", RequestedModel: "devin/swe-2", ChunkIndex: 2, Body: []byte(body),
		}, &resp)
		if len(resp.Body) > 0 {
			return gjson.ParseBytes(resp.Body)
		}
		return gjson.Parse(body)
	}
	first := send(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"a","type":"function","function":{"name":"x","arguments":""}}]},"finish_reason":null}]}`)
	second := send(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"id":"b","type":"function","function":{"name":"y","arguments":""}}]},"finish_reason":null}]}`)
	again := send(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{}"}}]},"finish_reason":null}]}`)
	if first.Get("choices.0.delta.tool_calls.0.index").Int() != 0 || second.Get("choices.0.delta.tool_calls.0.index").Int() != 1 || again.Get("choices.0.delta.tool_calls.0.index").Int() != 0 {
		t.Fatalf("indexes not normalized: %s | %s | %s", first.Raw, second.Raw, again.Raw)
	}
	send(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
	statesMu.Lock()
	_, left := states["chat-1"]
	statesMu.Unlock()
	if left {
		t.Fatalf("chat state must be dropped after finish_reason")
	}
}
