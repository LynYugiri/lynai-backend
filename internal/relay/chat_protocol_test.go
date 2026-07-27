package relay

import (
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/database"
)

func TestCanonicalChatFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/canonical_chat.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Request  json.RawMessage  `json:"request"`
		Response ChatResponse     `json:"response"`
		SSE      []canonicalDelta `json:"sse"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	request, err := parseCanonicalChat(fixture.Request)
	if err != nil {
		t.Fatalf("parse fixture request: %v", err)
	}
	if !request.Reasoning.Enabled || request.Reasoning.BudgetTokens == nil || *request.Reasoning.BudgetTokens != 2048 {
		t.Fatalf("fixture reasoning = %#v", request.Reasoning)
	}
	var toolChoice struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(request.ToolChoice, &toolChoice); err != nil {
		t.Fatal(err)
	}
	if len(request.Tools) != 1 || request.Tools[0].Name != "weather" || toolChoice.Name != "weather" {
		t.Fatalf("fixture tools/toolChoice = %#v/%s", request.Tools, request.ToolChoice)
	}
	if fixture.Response.Message.Content != "sunny" || fixture.Response.FinishReason != "stop" {
		t.Fatalf("fixture response = %#v", fixture.Response)
	}
	if len(fixture.SSE) != 3 || fixture.SSE[0].Sequence != 1 || fixture.SSE[2].Sequence != 3 || fixture.SSE[2].ResponseID != "response-1" || fixture.SSE[2].Type != "tool_calls" || fixture.SSE[2].Usage == nil || fixture.SSE[2].Usage.TotalTokens != 15 || !fixture.SSE[2].Done || fixture.SSE[2].FinishReason != "stop" {
		t.Fatalf("fixture SSE = %#v", fixture.SSE)
	}
}

func TestOpenAIStreamingRequestMergesUsageOptionAndProtectsCoreFields(t *testing.T) {
	request, err := parseCanonicalChat([]byte(`{
		"model":"test","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"stream":true,
		"extraParams":{"model":"evil","stream":false,"tools":[{"evil":true}],"thinking":{"type":"disabled"},"parallel_tool_calls":false,"stream_options":{"include_usage":false,"vendor_flag":"kept"}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := (openAIChatAdapter{}).Request(request)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	streamOptions := payload["stream_options"].(map[string]interface{})
	if payload["model"] != "test" || payload["stream"] != true || payload["parallel_tool_calls"] != false || payload["tools"] != nil || payload["thinking"] != nil || streamOptions["include_usage"] != true || streamOptions["vendor_flag"] != "kept" {
		t.Fatalf("OpenAI payload = %#v", payload)
	}
}

func TestOpenAIStreamingRequestRejectsNonObjectStreamOptions(t *testing.T) {
	request := ChatRequest{Model: "test", Stream: true, ExtraParams: map[string]interface{}{"stream_options": "invalid"}}
	if _, err := (openAIChatAdapter{}).Request(request); err == nil || !strings.Contains(err.Error(), "stream_options must be an object") {
		t.Fatalf("Request() error = %v", err)
	}
}

func TestOpenAIRequestConvertsCanonicalNamedToolChoice(t *testing.T) {
	request, err := parseCanonicalChat([]byte(`{
		"model":"test","messages":[{"role":"user","content":[{"type":"text","text":"weather?"}]}],
		"reasoning":{"enabled":true},
		"tools":[{"name":"weather","description":"Get weather","parameters":{"type":"object"}}],
		"toolChoice":{"name":"weather"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := (openAIChatAdapter{}).Request(request)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	want := map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "weather"}}
	if !reflect.DeepEqual(payload["tool_choice"], want) {
		t.Fatalf("tool_choice = %#v, want %#v", payload["tool_choice"], want)
	}
}

func TestCanonicalToolChoiceRejectsUnsupportedShapes(t *testing.T) {
	for _, choice := range []string{`"invalid"`, `{"type":"function"}`, `[]`, `1`} {
		raw := `{"model":"test","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"toolChoice":` + choice + `}`
		if _, err := parseCanonicalChat([]byte(raw)); err == nil {
			t.Fatalf("accepted toolChoice %s", choice)
		}
	}
}

func TestCanonicalChatRejectsStringContentAndInvalidFiles(t *testing.T) {
	tests := []string{
		`{"model":"test","messages":[{"role":"user","content":"legacy"}]}`,
		`{"model":"test","messages":[{"role":"user"}]}`,
		`{"model":"test","messages":[{"role":"user","content":null}]}`,
		`{"model":"test","messages":[{"role":"user","content":[{"type":"inputFile","file":{"name":"","mimeType":"image/png","dataBase64":"aQ=="}}]}]}`,
		`{"model":"test","messages":[{"role":"user","content":[{"type":"inputFile","file":{"name":"x.png","mimeType":"image/png; charset=binary","dataBase64":"aQ=="}}]}]}`,
		`{"model":"test","messages":[{"role":"user","content":[{"type":"inputFile","file":{"name":"x.svg","mimeType":"image/svg+xml","dataBase64":"aQ=="}}]}]}`,
		`{"model":"test","messages":[{"role":"user","content":[{"type":"inputFile","file":{"name":"x.png","mimeType":"image/png","dataBase64":"%%%"}}]}]}`,
		`{"model":"test","messages":[{"role":"user","content":[{"type":"inputFile","file":{"name":"x.png","mimeType":"image/png","dataBase64":"aQ==\n"}}]}]}`,
	}
	for _, raw := range tests {
		if _, err := parseCanonicalChat([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid request %s", raw)
		}
	}
}

func TestAdaptersMapCanonicalMultimodalContent(t *testing.T) {
	request, err := parseCanonicalChat([]byte(`{
		"model":"test","messages":[{"role":"user","content":[
			{"type":"text","text":"inspect"},
			{"type":"inputFile","file":{"name":"pixel.png","mimeType":"image/png","dataBase64":"aW1hZ2U="}},
			{"type":"inputFile","file":{"name":"notes.txt","mimeType":"text/plain","dataBase64":"aGVsbG8="}}
		]}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	fileLabel := "[文件: notes.txt (text/plain)]\ndata:text/plain;base64,aGVsbG8="
	tests := []struct {
		name    string
		adapter chatAdapter
		check   func(*testing.T, map[string]interface{})
	}{
		{"OpenAI", openAIChatAdapter{}, func(t *testing.T, payload map[string]interface{}) {
			content := payload["messages"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})
			if content[1].(map[string]interface{})["image_url"].(map[string]interface{})["url"] != "data:image/png;base64,aW1hZ2U=" || content[2].(map[string]interface{})["text"] != fileLabel {
				t.Fatalf("OpenAI content = %#v", content)
			}
		}},
		{"Anthropic", anthropicChatAdapter{}, func(t *testing.T, payload map[string]interface{}) {
			content := payload["messages"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})
			source := content[1].(map[string]interface{})["source"].(map[string]interface{})
			if source["media_type"] != "image/png" || source["data"] != "aW1hZ2U=" || content[2].(map[string]interface{})["text"] != fileLabel {
				t.Fatalf("Anthropic content = %#v", content)
			}
		}},
		{"Ollama", ollamaChatAdapter{}, func(t *testing.T, payload map[string]interface{}) {
			message := payload["messages"].([]interface{})[0].(map[string]interface{})
			if message["content"] != "inspect\n\n"+fileLabel || !reflect.DeepEqual(message["images"], []interface{}{"aW1hZ2U="}) {
				t.Fatalf("Ollama message = %#v", message)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := test.adapter.Request(request)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]interface{}
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatal(err)
			}
			test.check(t, payload)
		})
	}
}

func TestAdaptersRejectPrematureStreamEOF(t *testing.T) {
	tests := []struct {
		name    string
		adapter chatAdapter
		stream  string
		want    string
	}{
		{"OpenAI", openAIChatAdapter{}, `data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n\n", "before [DONE] or finish_reason"},
		{"Anthropic", anthropicChatAdapter{}, `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}` + "\n\n", "before message_stop"},
		{"Ollama", ollamaChatAdapter{}, `{"message":{"content":"partial"},"done":false}` + "\n", "before done=true"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.adapter.Stream(strings.NewReader(test.stream), func(canonicalDelta) error { return nil })
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Stream() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAdaptersAcceptRequiredCompletionMarkers(t *testing.T) {
	tests := []struct {
		name    string
		adapter chatAdapter
		stream  string
	}{
		{"OpenAIDONE", openAIChatAdapter{}, "data: [DONE]\n\n"},
		{"OpenAIFinishReason", openAIChatAdapter{}, `data: {"choices":[{"finish_reason":"stop","delta":{}}]}` + "\n\n"},
		{"Anthropic", anthropicChatAdapter{}, `data: {"type":"message_stop"}` + "\n\n"},
		{"Ollama", ollamaChatAdapter{}, `{"done":true,"done_reason":"stop"}` + "\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var deltas []canonicalDelta
			err := test.adapter.Stream(strings.NewReader(test.stream), func(delta canonicalDelta) error {
				deltas = append(deltas, delta)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(deltas) == 0 || !deltas[len(deltas)-1].Done {
				t.Fatalf("terminal deltas = %#v", deltas)
			}
		})
	}
}

func TestOpenAIStreamSortsSparseToolIndices(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":4,"id":"call-b","function":{"name":"second","arguments":"{\"b\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call-a","function":{"name":"first","arguments":"{\"a\":1}"}},{"index":4,"function":{"arguments":"2}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n")
	var deltas []canonicalDelta
	if err := (openAIChatAdapter{}).Stream(strings.NewReader(stream), func(delta canonicalDelta) error {
		deltas = append(deltas, delta)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	calls := deltas[len(deltas)-1].ToolCalls
	if len(calls) != 2 || calls[0].Name != "first" || calls[1].Name != "second" || string(calls[1].Arguments) != `{"b":2}` {
		t.Fatalf("sorted calls = %#v", calls)
	}
}

func TestOpenAIStreamCapturesResponseIDAndTerminalUsage(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"hello"}}]}`,
		`data: {"id":"chatcmpl-1","choices":[{"finish_reason":"stop","delta":{}}]}`,
		`data: {"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`,
		`data: [DONE]`,
		``,
	}, "\n\n")
	var deltas []canonicalDelta
	if err := (openAIChatAdapter{}).Stream(strings.NewReader(stream), func(delta canonicalDelta) error {
		deltas = append(deltas, delta)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	terminal := deltas[len(deltas)-1]
	if deltas[0].ResponseID != "chatcmpl-1" || terminal.ResponseID != "chatcmpl-1" || terminal.Usage == nil || terminal.Usage.InputTokens != 4 || terminal.Usage.OutputTokens != 2 || terminal.Usage.TotalTokens != 6 {
		t.Fatalf("OpenAI deltas = %#v", deltas)
	}
}

func TestAnthropicRealToolStreamUsesDeltaJSONTagsAndSparseIndices(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":2,"cache_read_input_tokens":3}}}`,
		"event: content_block_start\ndata: " + `{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_b","name":"second","input":{}}}`,
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"b\":2}"}}`,
		"event: content_block_start\ndata: " + `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_a","name":"first","input":{}}}`,
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
		"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":10}}`,
		"event: message_stop\ndata: " + `{"type":"message_stop"}`,
		``,
	}, "\n\n")
	var deltas []canonicalDelta
	if err := (anthropicChatAdapter{}).Stream(strings.NewReader(stream), func(delta canonicalDelta) error {
		deltas = append(deltas, delta)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	terminal := deltas[len(deltas)-1]
	if terminal.ResponseID != "msg_1" || terminal.Usage == nil || terminal.Usage.InputTokens != 6 || terminal.Usage.OutputTokens != 10 || terminal.Usage.TotalTokens != 16 || terminal.FinishReason != "tool_use" || len(terminal.ToolCalls) != 2 || terminal.ToolCalls[0].Name != "first" || terminal.ToolCalls[1].Name != "second" {
		t.Fatalf("terminal delta = %#v", terminal)
	}
}

func TestOllamaStreamCapturesTerminalUsage(t *testing.T) {
	stream := strings.Join([]string{
		`{"message":{"content":"hello"},"done":false}`,
		`{"done":true,"done_reason":"stop","prompt_eval_count":5,"eval_count":3}`,
		``,
	}, "\n")
	var deltas []canonicalDelta
	if err := (ollamaChatAdapter{}).Stream(strings.NewReader(stream), func(delta canonicalDelta) error {
		deltas = append(deltas, delta)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	terminal := deltas[len(deltas)-1]
	if terminal.Usage == nil || terminal.Usage.InputTokens != 5 || terminal.Usage.OutputTokens != 3 || terminal.Usage.TotalTokens != 8 {
		t.Fatalf("Ollama terminal delta = %#v", terminal)
	}
}

func TestCanonicalDefaultsMapAllSavedGenerationOptions(t *testing.T) {
	maxTokens, seed := 512, 42
	temperature, topP, presence, frequency := 0.2, 0.8, 0.1, -0.1
	user := "saved-user"
	request := ChatRequest{}
	applyCanonicalDefaults(&request, database.RelayModel{AdvancedParams: EncodeAdvancedParams(ModelAdvancedParams{
		MaxTokens: &maxTokens, Temperature: &temperature, TopP: &topP,
		PresencePenalty: &presence, FrequencyPenalty: &frequency, Seed: &seed,
		Stop: []string{"END"}, User: &user,
	})})
	raw, err := (openAIChatAdapter{}).Request(request)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]interface{}{
		"max_tokens": float64(512), "temperature": 0.2, "top_p": 0.8,
		"presence_penalty": 0.1, "frequency_penalty": -0.1, "seed": float64(42), "user": "saved-user",
	} {
		if payload[key] != want {
			t.Errorf("%s = %#v, want %#v", key, payload[key], want)
		}
	}
	if !reflect.DeepEqual(payload["stop"], []interface{}{"END"}) {
		t.Fatalf("stop = %#v", payload["stop"])
	}

	anthropicRaw, err := (anthropicChatAdapter{}).Request(request)
	if err != nil {
		t.Fatal(err)
	}
	var anthropicPayload map[string]interface{}
	_ = json.Unmarshal(anthropicRaw, &anthropicPayload)
	if !reflect.DeepEqual(anthropicPayload["stop_sequences"], []interface{}{"END"}) {
		t.Fatalf("Anthropic stop_sequences = %#v", anthropicPayload["stop_sequences"])
	}

	ollamaRaw, err := (ollamaChatAdapter{}).Request(request)
	if err != nil {
		t.Fatal(err)
	}
	var ollamaPayload map[string]interface{}
	_ = json.Unmarshal(ollamaRaw, &ollamaPayload)
	if !reflect.DeepEqual(ollamaPayload["options"].(map[string]interface{})["stop"], []interface{}{"END"}) {
		t.Fatalf("Ollama stop = %#v", ollamaPayload)
	}
}

func TestNonStreamingAdaptersRejectMissingProtocolMarkers(t *testing.T) {
	tests := []struct {
		name    string
		adapter chatAdapter
		body    string
	}{
		{"OpenAIEmpty", openAIChatAdapter{}, `{}`},
		{"OpenAIMissingMessage", openAIChatAdapter{}, `{"choices":[{"finish_reason":"stop"}]}`},
		{"OpenAIMissingFinishReason", openAIChatAdapter{}, `{"choices":[{"message":{"content":"ok"}}]}`},
		{"AnthropicEmpty", anthropicChatAdapter{}, `{}`},
		{"AnthropicMissingContent", anthropicChatAdapter{}, `{"stop_reason":"end_turn"}`},
		{"AnthropicMissingStopReason", anthropicChatAdapter{}, `{"content":[]}`},
		{"OllamaEmpty", ollamaChatAdapter{}, `{}`},
		{"OllamaMissingMessage", ollamaChatAdapter{}, `{"done":true}`},
		{"OllamaNotDone", ollamaChatAdapter{}, `{"message":{"content":"ok"},"done":false}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.adapter.Response([]byte(test.body)); err == nil {
				t.Fatalf("accepted %s", test.body)
			}
		})
	}
}

func TestNonAnthropicAdaptersRejectBudgetTokens(t *testing.T) {
	budget := 2048
	request := ChatRequest{Reasoning: ChatReasoning{Enabled: true, BudgetTokens: &budget}}
	for _, adapter := range []chatAdapter{openAIChatAdapter{}, ollamaChatAdapter{}} {
		if _, err := adapter.Request(request); err == nil || !strings.Contains(err.Error(), "budgetTokens") {
			t.Fatalf("Request() error = %v", err)
		}
	}
	if _, err := (anthropicChatAdapter{}).Request(request); err != nil {
		t.Fatalf("Anthropic rejected budgetTokens: %v", err)
	}
}

func TestAnthropicReasoningBudgetStaysBelowMaxTokens(t *testing.T) {
	for _, test := range []struct {
		name       string
		maxTokens  int
		budget     *int
		wantBudget int
	}{
		{name: "default budget", maxTokens: 512, wantBudget: 511},
		{name: "configured budget", maxTokens: 100, budget: intPointer(80), wantBudget: 80},
		{name: "configured budget clamped", maxTokens: 100, budget: intPointer(1000), wantBudget: 99},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := ChatRequest{
				Model: "claude",
				Messages: []ChatMessage{{
					Role:    "user",
					Content: []ChatContentPart{{Type: "text", Text: "hello"}},
				}},
				MaxTokens: &test.maxTokens,
				Reasoning: ChatReasoning{Enabled: true, BudgetTokens: test.budget},
			}
			raw, err := (anthropicChatAdapter{}).Request(request)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]interface{}
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatal(err)
			}
			thinking := payload["thinking"].(map[string]interface{})
			if thinking["budget_tokens"] != float64(test.wantBudget) {
				t.Fatalf("budget_tokens = %v, want %d", thinking["budget_tokens"], test.wantBudget)
			}
		})
	}
}

func TestAnthropicReasoningRejectsMaxTokensWithoutBudgetRoom(t *testing.T) {
	maxTokens := 1
	request := ChatRequest{
		Model: "claude",
		Messages: []ChatMessage{{
			Role:    "user",
			Content: []ChatContentPart{{Type: "text", Text: "hello"}},
		}},
		MaxTokens: &maxTokens,
		Reasoning: ChatReasoning{Enabled: true},
	}
	if _, err := (anthropicChatAdapter{}).Request(request); err == nil || !strings.Contains(err.Error(), "maxTokens greater than one") {
		t.Fatalf("Request() error = %v", err)
	}
}

func intPointer(value int) *int { return &value }

type errorStreamAdapter struct{ err error }

func (errorStreamAdapter) Request(ChatRequest) ([]byte, error)                  { return nil, nil }
func (errorStreamAdapter) Response([]byte) (ChatResponse, error)                { return ChatResponse{}, nil }
func (a errorStreamAdapter) Stream(io.Reader, func(canonicalDelta) error) error { return a.err }

func TestWriteCanonicalSSEClassifiesTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"timeout", ErrUpstreamTimeout, "upstream_timeout"},
		{"protocol", errors.New("bad event"), "upstream_protocol_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			if err := writeCanonicalSSE(ctx, errorStreamAdapter{err: test.err}, strings.NewReader("")); !errors.Is(err, test.err) {
				t.Fatalf("writeCanonicalSSE error = %v, want %v", err, test.err)
			}
			if got := ctx.GetString("relayErrorType"); got != test.want {
				t.Fatalf("relayErrorType = %q, want %q", got, test.want)
			}
			body := recorder.Body.String()
			if !strings.Contains(body, "event: chunk") || !strings.Contains(body, `"sequence":1`) || !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, `"errorInfo":{"type":"`+test.want+`"`) {
				t.Fatalf("canonical error event = %s", body)
			}
		})
	}
}

type deltaStreamAdapter struct{ deltas []canonicalDelta }

func (deltaStreamAdapter) Request(ChatRequest) ([]byte, error)   { return nil, nil }
func (deltaStreamAdapter) Response([]byte) (ChatResponse, error) { return ChatResponse{}, nil }
func (a deltaStreamAdapter) Stream(_ io.Reader, emit func(canonicalDelta) error) error {
	for _, delta := range a.deltas {
		if err := emit(delta); err != nil {
			return err
		}
	}
	return nil
}

func TestWriteCanonicalSSEAddsStableTypedMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	usage := &ChatUsage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}
	err := writeCanonicalSSE(ctx, deltaStreamAdapter{deltas: []canonicalDelta{
		{ResponseID: "upstream-1", Content: "hello"},
		{ResponseID: "ignored-late-id", Reasoning: "think"},
		{ToolCalls: []ChatToolCall{{ID: "call-1", Name: "weather", Arguments: json.RawMessage(`{}`)}}, FinishReason: "tool_calls", Usage: usage, Done: true},
	}}, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"sequence":1,"responseId":"upstream-1","type":"content"`,
		`"sequence":2,"responseId":"upstream-1","type":"reasoning"`,
		`"sequence":3,"responseId":"upstream-1","type":"tool_calls"`,
		`"usage":{"inputTokens":2,"outputTokens":3,"totalTokens":5}`,
	} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("canonical stream missing %s: %s", want, recorder.Body.String())
		}
	}
}

type failingResponseWriter struct {
	gin.ResponseWriter
	err error
}

func (w failingResponseWriter) Write([]byte) (int, error) { return 0, w.err }

func TestWriteCanonicalSSEClassifiesDownstreamWriteFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	wantErr := errors.New("client disconnected")
	ctx.Writer = failingResponseWriter{ResponseWriter: ctx.Writer, err: wantErr}
	err := writeCanonicalSSE(ctx, deltaStreamAdapter{deltas: []canonicalDelta{{Content: "hello"}}}, strings.NewReader(""))
	if !errors.Is(err, wantErr) || !isDownstreamWriteError(err) {
		t.Fatalf("writeCanonicalSSE error = %v", err)
	}
	if got := ctx.GetString("relayErrorType"); got != "client_disconnect" {
		t.Fatalf("relayErrorType = %q", got)
	}
}
