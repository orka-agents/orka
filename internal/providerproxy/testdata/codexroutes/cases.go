// Package codexroutes contains literal wire fixtures shared by the two HTTP
// hop tests. Do not construct these with json.Marshal: duplicate members and
// the order of colliding selectors are precisely what the tests exercise.
package codexroutes

type Case struct {
	Name, Body, Header string
	Qualified          bool
	InvalidJSON        bool
}

func Cases() []Case {
	const prefix = `{"model":"test-model",`
	return []Case{
		{Name: "canonical", Body: prefix + `"stream":true}`, Qualified: true},
		{Name: "spaced_literal_with_opaque_data", Body: `{ "model" : "test-model", "stream" : true, "metadata":{"\u0073tream":false}, "input":"\u0073tream is data" }`, Qualified: true},
		{Name: "tool_result", Body: prefix + `"stream":true,"input":[{"type":"function_call_output","call_id":"read-1","output":"{\"stream\":false}"}]}`, Qualified: true},
		{Name: "ordinary_nested_data", Body: prefix + `"stream":true,"store":false,"background":false,"input":[{"type":"function_call","call_id":"a","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"compaction_trigger"}],"tools":[{"type":"function","name":"read","parameters":{"properties":{"conversation":{"type":"string"}}}}]}`, Qualified: true},
		{Name: "missing", Body: `{"model":"test-model"}`},
		{Name: "false", Body: prefix + `"stream":false}`},
		{Name: "null", Body: prefix + `"stream":null}`},
		{Name: "string", Body: prefix + `"stream":"true"}`},
		{Name: "number", Body: prefix + `"stream":1}`},
		{Name: "wrong_case", Body: prefix + `"Stream":true}`},
		{Name: "standalone_escaped_stream", Body: prefix + `"\u0073tream":true}`},
		{Name: "false_then_true", Body: prefix + `"stream":false,"stream":true}`},
		{Name: "true_then_false", Body: prefix + `"stream":true,"stream":false}`},
		{Name: "duplicate_true", Body: prefix + `"stream":true,"stream":true}`},
		{Name: "case_collision", Body: prefix + `"Stream":false,"stream":true}`},
		{Name: "reverse_case_collision", Body: prefix + `"stream":true,"STREAM":false}`},
		{Name: "escaped_duplicate", Body: prefix + `"\u0073tream":true,"stream":true}`},
		{Name: "model_collision", Body: `{"model":"other","model":"test-model","stream":true}`},
		{Name: "escaped_model_collision", Body: `{"m\u006fdel":"other","model":"test-model","stream":true}`},
		{Name: "compact_trigger", Body: prefix + `"stream":true,"input":[{"type":"compaction_trigger"}]}`},
		{Name: "hidden_trigger", Body: prefix + `"stream":true,"input":[{"type":"compaction_trigger","type":"message"}]}`},
		{Name: "escaped_compaction", Body: prefix + `"stream":true,"input":[{"type":"c\u006fmpaction"}]}`},
		{Name: "escaped_item_selector", Body: prefix + `"stream":true,"input":[{"ty\u0070e":"message","role":"user","content":"read"}]}`},
		{Name: "context_compaction", Body: prefix + `"stream":true,"input":[{"type":"context_compaction"}]}`},
		{Name: "item_reference", Body: prefix + `"stream":true,"input":[{"type":"item_reference","id":"some-item"}]}`},
		{Name: "conversation", Body: prefix + `"stream":true,"conversation":"fixture-conversation"}`},
		{Name: "previous_response", Body: prefix + `"stream":true,"previous_response_id":"resp-other"}`},
		{Name: "context_management", Body: prefix + `"stream":true,"context_management":[{"type":"compaction"}]}`},
		{Name: "background", Body: prefix + `"stream":true,"background":true}`},
		{Name: "store", Body: prefix + `"stream":true,"store":true}`},
		{Name: "history_override", Body: prefix + `"stream":true}`, Header: "X-Vekil-History-Complete"},
		{Name: "turn_state", Body: prefix + `"stream":true}`, Header: "X-Codex-Turn-State"},
		{Name: "method_override", Body: prefix + `"stream":true}`, Header: "X-HTTP-Method-Override"},
		{Name: "path_override", Body: prefix + `"stream":true}`, Header: "X-Original-URL"},
		{Name: "websocket_upgrade", Body: prefix + `"stream":true}`, Header: "Upgrade"},
		{Name: "trailing_json", Body: prefix + `"stream":true} {"stream":false}`, InvalidJSON: true},
	}
}
