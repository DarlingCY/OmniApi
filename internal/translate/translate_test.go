package translate

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/omniapi/omni-api/internal/model"
)

var protocols = []model.Protocol{model.ProtocolOpenAIChat, model.ProtocolOpenAIResponses, model.ProtocolAnthropic}

func decode(t *testing.T, s string) object {
	t.Helper()
	var b object
	if err := json.Unmarshal([]byte(s), &b); err != nil {
		t.Fatal(err)
	}
	return b
}

var requests = map[model.Protocol]string{
	model.ProtocolOpenAIChat:      `{"model":"public","max_completion_tokens":2048,"messages":[{"role":"system","content":"Be useful"},{"role":"user","content":"Weather?"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_a","type":"function","function":{"name":"weather","arguments":"{\"city\":\"杭州\"}"}}]},{"role":"tool","tool_call_id":"call_a","content":"sunny"}],"tools":[{"type":"function","function":{"name":"weather","description":"Look up weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],"tool_choice":{"type":"function","function":{"name":"weather"}},"parallel_tool_calls":false}`,
	model.ProtocolOpenAIResponses: `{"model":"public","max_output_tokens":2048,"instructions":"Be useful","input":[{"role":"user","content":"Weather?"},{"type":"function_call","call_id":"call_a","name":"weather","arguments":"{\"city\":\"杭州\"}"},{"type":"function_call_output","call_id":"call_a","output":"sunny"}],"tools":[{"type":"function","name":"weather","description":"Look up weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],"tool_choice":{"type":"function","name":"weather"},"parallel_tool_calls":false}`,
	model.ProtocolAnthropic:       `{"model":"public","max_tokens":2048,"system":[{"type":"text","text":"Be useful"}],"messages":[{"role":"user","content":[{"type":"text","text":"Weather?"}]},{"role":"assistant","content":[{"type":"tool_use","id":"call_a","name":"weather","input":{"city":"杭州"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_a","content":"sunny"}]}],"tools":[{"name":"weather","description":"Look up weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],"tool_choice":{"type":"tool","name":"weather","disable_parallel_tool_use":true}}`,
}

func TestRequestMatrixPreservesToolRoundTrip(t *testing.T) {
	for _, from := range protocols {
		for _, to := range protocols {
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				input := decode(t, requests[from])
				before, _ := json.Marshal(input)
				out, err := Request(from, to, input, "actual-model")
				if err != nil {
					t.Fatal(err)
				}
				after, _ := json.Marshal(input)
				if string(before) != string(after) {
					t.Fatal("request mutated")
				}
				if out["model"] != "actual-model" {
					t.Fatal("model alias not resolved")
				}
				tools := arr(out["tools"])
				if len(tools) != 1 {
					t.Fatal("tools missing")
				}
				tool := obj(tools[0])
				schemaKey := "parameters"
				choice := obj(out["tool_choice"])
				switch to {
				case model.ProtocolOpenAIChat:
					if tool["type"] != "function" {
						t.Fatal("missing function type")
					}
					tool = obj(tool["function"])
					choice = obj(choice["function"])
				case model.ProtocolOpenAIResponses:
					if tool["type"] != "function" {
						t.Fatal("missing function type")
					}
				default:
					schemaKey = "input_schema"
					if choice["type"] != "tool" || choice["disable_parallel_tool_use"] != true {
						t.Fatal("tool choice semantics lost")
					}
				}
				if tool["name"] != "weather" || choice["name"] != "weather" {
					t.Fatal("tool name/selection lost")
				}
				if str(obj(obj(obj(tool[schemaKey])["properties"])["city"])["type"]) != "string" {
					t.Fatal("input schema lost")
				}
				if to != model.ProtocolAnthropic && out["parallel_tool_calls"] != false {
					t.Fatal("parallel choice lost")
				}
				// Validate the actual wire schema, including call/result correlation.
				callID, resultID, args, result := "", "", "", ""
				key := "messages"
				if to == model.ProtocolOpenAIResponses {
					key = "input"
				}
				for _, v := range arr(out[key]) {
					m := obj(v)
					if to == model.ProtocolOpenAIResponses {
						if m["type"] == "function_call" {
							callID = str(m["call_id"])
							args = str(m["arguments"])
						}
						if m["type"] == "function_call_output" {
							resultID = str(m["call_id"])
							result = jsonText(m["output"])
						}
					} else if to == model.ProtocolOpenAIChat {
						for _, v := range arr(m["tool_calls"]) {
							c := obj(v)
							callID = str(c["id"])
							args = str(obj(c["function"])["arguments"])
						}
						if m["role"] == "tool" {
							resultID = str(m["tool_call_id"])
							result = jsonText(m["content"])
						}
					} else {
						for _, v := range arr(m["content"]) {
							b := obj(v)
							if b["type"] == "tool_use" {
								callID = str(b["id"])
								args = jsonText(b["input"])
							}
							if b["type"] == "tool_result" {
								resultID = str(b["tool_use_id"])
								result = jsonText(b["content"])
							}
						}
					}
				}
				if callID != "call_a" || resultID != callID || str(decode(t, args)["city"]) != "杭州" || !strings.Contains(result, "sunny") {
					t.Fatalf("tool conversation corrupted: %#v", out)
				}
				encoded, _ := json.Marshal(out)
				if !strings.Contains(string(encoded), "Be useful") {
					t.Fatal("system instruction lost")
				}
			})
		}
	}
}

var completions = map[model.Protocol]string{
	model.ProtocolOpenAIChat:      `{"id":"upstream","model":"actual-model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_a","type":"function","function":{"name":"weather","arguments":"{\"city\":\"杭州\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":3}}}`,
	model.ProtocolOpenAIResponses: `{"id":"upstream","model":"actual-model","status":"completed","output":[{"type":"function_call","id":"fc_a","call_id":"call_a","name":"weather","arguments":"{\"city\":\"杭州\"}"}],"usage":{"input_tokens":10,"output_tokens":4,"input_tokens_details":{"cached_tokens":3}}}`,
	model.ProtocolAnthropic:       `{"id":"upstream","type":"message","role":"assistant","model":"actual-model","content":[{"type":"tool_use","id":"call_a","name":"weather","input":{"city":"杭州"}}],"stop_reason":"tool_use","usage":{"input_tokens":7,"output_tokens":4,"cache_read_input_tokens":3}}`,
}

func TestResponseMatrixIncludesToolOnlyResults(t *testing.T) {
	for _, from := range protocols {
		for _, to := range protocols {
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				out, err := Response(from, to, decode(t, completions[from]), "public", "req", object{})
				if err != nil {
					t.Fatal(err)
				}
				if out["model"] != "public" {
					t.Fatal("upstream model leaked")
				}
				var call object
				switch to {
				case model.ProtocolOpenAIChat:
					c := obj(arr(out["choices"])[0])
					if c["finish_reason"] != "tool_calls" {
						t.Fatal("tool stop reason lost")
					}
					call = obj(arr(obj(c["message"])["tool_calls"])[0])
					if call["id"] != "call_a" {
						t.Fatal("call id lost")
					}
					call = obj(call["function"])
				case model.ProtocolOpenAIResponses:
					call = obj(arr(out["output"])[0])
					if call["type"] != "function_call" || call["call_id"] != "call_a" {
						t.Fatal("tool item lost")
					}
				default:
					call = obj(arr(out["content"])[0])
					if out["stop_reason"] != "tool_use" || call["id"] != "call_a" {
						t.Fatal("tool use lost")
					}
				}
				if call["name"] != "weather" {
					t.Fatal("name lost")
				}
				if got := TotalUsage(to, out); got.InputTokens != 10 || got.OutputTokens != 4 || got.CachedTokens != 3 {
					t.Fatalf("usage changed: %#v", got)
				}
			})
		}
	}
}

func TestSameProtocolPreservesExtensions(t *testing.T) {
	for _, p := range protocols {
		input := decode(t, `{"model":"public","tools":[{"type":"future_native_tool"}],"conversation":"saved","metadata":{"label":"hello"},"unknown_option":true,"input":[{"type":"image","file_id":"file_1"}]}`)
		out, err := Request(p, p, input, "actual")
		if err != nil {
			t.Fatal(err)
		}
		expected := clone(input)
		expected["model"] = "actual"
		if !reflect.DeepEqual(out, expected) {
			t.Fatal("same-protocol extensions dropped")
		}
	}
}

func TestUnsupportedFeaturesFailBeforeForwarding(t *testing.T) {
	for _, input := range []string{`{"tools":[{"type":"web_search"}]}`, `{"previous_response_id":"resp_old"}`, `{"input":[{"role":"user","content":[{"type":"input_audio","data":"encoded"}]}]}`} {
		if _, err := Request(model.ProtocolOpenAIResponses, model.ProtocolAnthropic, decode(t, input), "m"); err == nil {
			t.Fatalf("silently accepted unsupported payload: %s", input)
		}
	}
}

func TestMultimodalMessagesAreNotStringified(t *testing.T) {
	for _, to := range []model.Protocol{model.ProtocolOpenAIChat, model.ProtocolOpenAIResponses} {
		out, err := Request(model.ProtocolAnthropic, to, decode(t, `{"system":[{"type":"text","text":"A"},{"type":"text","text":"B"}],"messages":[{"role":"user","content":[{"type":"text","text":"Look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}]}`), "m")
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(out)
		if !strings.Contains(string(encoded), "data:image/png;base64,aGVsbG8=") || strings.Contains(string(encoded), `\"type\"`) {
			t.Fatalf("content blocks damaged: %s", encoded)
		}
	}
}

func TestStreamMatrixRoundTrip(t *testing.T) {
	streamInputs := map[model.Protocol][]Frame{
		model.ProtocolOpenAIChat: {
			{Data: `{"choices":[{"delta":{"role":"assistant","content":""}}]}`},
			{Data: `{"choices":[{"delta":{"reasoning_content":"Thinking..."}}]}`},
			{Data: `{"choices":[{"delta":{"content":"Hello!"}}]}`},
			{Data: `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":\"go\"}"}}]}}]}`},
			{Data: `{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":20}}`},
			{Data: `[DONE]`},
		},
		model.ProtocolOpenAIResponses: {
			{Event: "response.output_item.added", Data: `{"type":"response.output_item.added","output_index":0,"item":{"id":"item_0","type":"reasoning","summary":[]}}`},
			{Event: "response.reasoning_summary_text.delta", Data: `{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"Thinking..."}`},
			{Event: "response.output_item.done", Data: `{"type":"response.output_item.done","output_index":0,"item":{"id":"item_0","type":"reasoning","summary":[{"type":"summary_text","text":"Thinking..."}]}}`},
			{Event: "response.output_item.added", Data: `{"type":"response.output_item.added","output_index":1,"item":{"id":"item_1","type":"message","role":"assistant","content":[]}}`},
			{Event: "response.output_text.delta", Data: `{"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"Hello!"}`},
			{Event: "response.output_item.done", Data: `{"type":"response.output_item.done","output_index":1,"item":{"id":"item_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello!"}]}}`},
			{Event: "response.output_item.added", Data: `{"type":"response.output_item.added","output_index":2,"item":{"id":"item_2","type":"function_call","call_id":"call_1","name":"search","arguments":""}}`},
			{Event: "response.function_call_arguments.delta", Data: `{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"q\":\"go\"}"}`},
			{Event: "response.output_item.done", Data: `{"type":"response.output_item.done","output_index":2,"item":{"id":"item_2","type":"function_call","call_id":"call_1","name":"search","arguments":"{\"q\":\"go\"}"}}`},
			{Event: "response.completed", Data: `{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":10,"output_tokens":20}}}`},
		},
		model.ProtocolAnthropic: {
			{Event: "message_start", Data: `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":10}}}`},
			{Event: "content_block_start", Data: `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`},
			{Event: "content_block_delta", Data: `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Thinking..."}}`},
			{Event: "content_block_stop", Data: `{"type":"content_block_stop","index":0}`},
			{Event: "content_block_start", Data: `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`},
			{Event: "content_block_delta", Data: `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hello!"}}`},
			{Event: "content_block_stop", Data: `{"type":"content_block_stop","index":1}`},
			{Event: "content_block_start", Data: `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_1","name":"search","input":{}}}`},
			{Event: "content_block_delta", Data: `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"go\"}"}}`},
			{Event: "content_block_stop", Data: `{"type":"content_block_stop","index":2}`},
			{Event: "message_delta", Data: `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}`},
			{Event: "message_stop", Data: `{"type":"message_stop"}`},
		},
	}

	for _, from := range protocols {
		for _, to := range protocols {
			t.Run("stream:"+string(from)+"->"+string(to), func(t *testing.T) {
				stream := NewStream(from, to, "target-model", "test-req", object{})
				var emitted []Frame
				for _, f := range streamInputs[from] {
					frames, err := stream.Push(f)
					if err != nil {
						t.Fatalf("push failed for %s->%s: %v (frame: %s)", from, to, err, f.Data)
					}
					emitted = append(emitted, frames...)
				}
				if !stream.Done() {
					endFrames, err := stream.End()
					if err != nil {
						t.Fatalf("end failed: %v", err)
					}
					emitted = append(emitted, endFrames...)
				}

				allData := ""
				for _, ef := range emitted {
					allData += ef.Data + "\n"
				}

				// Check that core information survived translation
				if !strings.Contains(allData, "Hello!") {
					t.Fatalf("%s->%s stream lost text payload: %s", from, to, allData)
				}
				if !strings.Contains(allData, "search") {
					t.Fatalf("%s->%s stream lost tool name: %s", from, to, allData)
				}
				if !strings.Contains(allData, "call_1") {
					t.Fatalf("%s->%s stream lost tool call ID: %s", from, to, allData)
				}
			})
		}
	}
}
