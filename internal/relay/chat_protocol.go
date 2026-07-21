package relay

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/database"
)

type ChatRequest struct {
	ProviderID       string          `json:"providerId"`
	Model            string          `json:"model"`
	Messages         []ChatMessage   `json:"messages"`
	Stream           bool            `json:"stream"`
	Reasoning        ChatReasoning   `json:"reasoning,omitempty"`
	Tools            []ChatTool      `json:"tools,omitempty"`
	ToolChoice       json.RawMessage `json:"toolChoice,omitempty"`
	MaxTokens        *int            `json:"maxTokens,omitempty"`
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"topP,omitempty"`
	PresencePenalty  *float64        `json:"presencePenalty,omitempty"`
	FrequencyPenalty *float64        `json:"frequencyPenalty,omitempty"`
	Seed             *int            `json:"seed,omitempty"`
	Stop             []string        `json:"stop,omitempty"`
	User             *string         `json:"user,omitempty"`
}

type ChatReasoning struct {
	Enabled      bool `json:"enabled"`
	BudgetTokens *int `json:"budgetTokens,omitempty"`
}

type ChatMessage struct {
	Role       string            `json:"role"`
	Content    []ChatContentPart `json:"content"`
	Reasoning  string            `json:"reasoning,omitempty"`
	ToolCalls  []ChatToolCall    `json:"toolCalls,omitempty"`
	ToolCallID string            `json:"toolCallId,omitempty"`
}

type ChatContentPart struct {
	Type string         `json:"type"`
	Text string         `json:"text,omitempty"`
	File *ChatInputFile `json:"file,omitempty"`
}

type ChatInputFile struct {
	Name       string `json:"name"`
	MIMEType   string `json:"mimeType"`
	DataBase64 string `json:"dataBase64"`
}

func (m ChatMessage) Text() string {
	var text strings.Builder
	for _, part := range m.Content {
		switch part.Type {
		case "text":
			text.WriteString(part.Text)
		case "inputFile":
			if text.Len() > 0 {
				text.WriteString("\n\n")
			}
			text.WriteString(fileText(*part.File))
		}
	}
	return text.String()
}

type ChatTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ChatToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ChatResponse struct {
	Message      ChatResponseMessage `json:"message"`
	FinishReason string              `json:"finishReason"`
}

type ChatResponseMessage struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	Reasoning string         `json:"reasoning,omitempty"`
	ToolCalls []ChatToolCall `json:"toolCalls,omitempty"`
}

type canonicalDelta struct {
	Content      string         `json:"content,omitempty"`
	Reasoning    string         `json:"reasoning,omitempty"`
	ToolCalls    []ChatToolCall `json:"toolCalls,omitempty"`
	FinishReason string         `json:"finishReason,omitempty"`
	Done         bool           `json:"done,omitempty"`
	Error        string         `json:"error,omitempty"`
}

type chatAdapter interface {
	Request(ChatRequest) ([]byte, error)
	Response([]byte) (ChatResponse, error)
	Stream(io.Reader, func(canonicalDelta) error) error
}

func parseCanonicalChat(raw []byte) (ChatRequest, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var request ChatRequest
	if err := decoder.Decode(&request); err != nil {
		return request, errors.New("invalid canonical chat JSON")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return request, errors.New("invalid canonical chat JSON")
	}
	request.ProviderID = strings.TrimSpace(request.ProviderID)
	request.Model = strings.TrimSpace(request.Model)
	if request.ProviderID == "" || request.Model == "" {
		return request, errors.New("providerId and model are required")
	}
	if len(request.Messages) == 0 {
		return request, errors.New("messages are required")
	}
	if request.Reasoning.BudgetTokens != nil && *request.Reasoning.BudgetTokens <= 0 {
		return request, errors.New("reasoning budgetTokens must be greater than zero")
	}
	for i := range request.Messages {
		message := &request.Messages[i]
		if message.Content == nil {
			return request, errors.New("message content must be an array")
		}
		switch message.Role {
		case "system", "user", "assistant":
		case "tool":
			if strings.TrimSpace(message.ToolCallID) == "" {
				return request, errors.New("tool messages require toolCallId")
			}
		default:
			return request, fmt.Errorf("unsupported message role %q", message.Role)
		}
		for j := range message.Content {
			if err := validateContentPart(message.Content[j]); err != nil {
				return request, fmt.Errorf("invalid message content: %w", err)
			}
		}
		for j := range message.ToolCalls {
			if err := validateToolCall(message.ToolCalls[j]); err != nil {
				return request, err
			}
		}
	}
	for _, tool := range request.Tools {
		if strings.TrimSpace(tool.Name) == "" || !validJSONObject(tool.Parameters) {
			return request, errors.New("tools require a name and JSON object parameters")
		}
	}
	if len(request.ToolChoice) > 0 && !validToolChoice(request.ToolChoice) {
		return request, errors.New("toolChoice must be a supported string or object")
	}
	for _, stop := range request.Stop {
		if stop == "" {
			return request, errors.New("stop entries must be non-empty")
		}
	}
	return request, nil
}

func validateContentPart(part ChatContentPart) error {
	switch part.Type {
	case "text":
		if part.File != nil {
			return errors.New("text parts must not contain file")
		}
	case "inputFile":
		if part.Text != "" || part.File == nil {
			return errors.New("inputFile parts require file and must not contain text")
		}
		part.File.Name = strings.TrimSpace(part.File.Name)
		part.File.MIMEType = strings.TrimSpace(part.File.MIMEType)
		if part.File.Name == "" || strings.ContainsAny(part.File.Name, "\r\n") {
			return errors.New("inputFile name must be non-empty and single-line")
		}
		mediaType, params, err := mime.ParseMediaType(part.File.MIMEType)
		if err != nil || len(params) != 0 || mediaType != part.File.MIMEType {
			return errors.New("inputFile mimeType must be a normalized MIME type without parameters")
		}
		if strings.HasPrefix(mediaType, "image/") && !supportedImageMIMEType(mediaType) {
			return errors.New("unsupported inputFile image mimeType")
		}
		decoded, err := base64.StdEncoding.DecodeString(part.File.DataBase64)
		if err != nil || len(decoded) == 0 || base64.StdEncoding.EncodeToString(decoded) != part.File.DataBase64 {
			return errors.New("inputFile dataBase64 must be valid base64")
		}
	default:
		return fmt.Errorf("unsupported content part type %q", part.Type)
	}
	return nil
}

func supportedImageMIMEType(value string) bool {
	switch value {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func fileText(file ChatInputFile) string {
	return fmt.Sprintf("[文件: %s (%s)]\ndata:%s;base64,%s", file.Name, file.MIMEType, file.MIMEType, file.DataBase64)
}

func validToolChoice(raw json.RawMessage) bool {
	var choice string
	if json.Unmarshal(raw, &choice) == nil {
		switch choice {
		case "auto", "none", "required", "any":
			return true
		default:
			return false
		}
	}
	var object struct {
		Name string `json:"name"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(&object) == nil && strings.TrimSpace(object.Name) != "" && decoder.Decode(&struct{}{}) == io.EOF
}

func validateToolCall(call ChatToolCall) error {
	if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" || !validJSONObject(call.Arguments) {
		return errors.New("tool calls require id, name, and JSON object arguments")
	}
	return nil
}

func validJSONObject(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var value map[string]interface{}
	return json.Unmarshal(raw, &value) == nil && value != nil
}

func applyCanonicalDefaults(request *ChatRequest, model database.RelayModel) {
	params := DecodeAdvancedParams(model.AdvancedParams)
	if request.MaxTokens == nil {
		request.MaxTokens = params.MaxTokens
	}
	if request.Temperature == nil {
		request.Temperature = params.Temperature
	}
	if request.TopP == nil {
		request.TopP = params.TopP
	}
	if request.PresencePenalty == nil {
		request.PresencePenalty = params.PresencePenalty
	}
	if request.FrequencyPenalty == nil {
		request.FrequencyPenalty = params.FrequencyPenalty
	}
	if request.Seed == nil {
		request.Seed = params.Seed
	}
	if request.Stop == nil && len(params.Stop) > 0 {
		request.Stop = append([]string(nil), params.Stop...)
	}
	if request.User == nil {
		request.User = params.User
	}
}

func requestUsesTools(request ChatRequest) bool {
	if len(request.Tools) > 0 {
		return true
	}
	for _, message := range request.Messages {
		if message.Role == "tool" || len(message.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

func requestUsesImages(request ChatRequest) bool {
	for _, message := range request.Messages {
		for _, part := range message.Content {
			if part.Type == "inputFile" && strings.HasPrefix(part.File.MIMEType, "image/") {
				return true
			}
		}
	}
	return false
}

func adapterFor(apiFormat string) (chatAdapter, error) {
	switch normalizeAPIType(apiFormat) {
	case APIFormatOpenAI:
		return openAIChatAdapter{}, nil
	case APIFormatAnthropic:
		return anthropicChatAdapter{}, nil
	case APIFormatOllama:
		return ollamaChatAdapter{}, nil
	default:
		return nil, errors.New("requested provider does not support chat")
	}
}

type openAIChatAdapter struct{}

func (openAIChatAdapter) Request(request ChatRequest) ([]byte, error) {
	if request.Reasoning.BudgetTokens != nil {
		return nil, errors.New("OpenAI adapter does not support reasoning budgetTokens")
	}
	messages := make([]map[string]interface{}, 0, len(request.Messages))
	for _, message := range request.Messages {
		item := map[string]interface{}{"role": message.Role, "content": openAIContent(message.Content)}
		if message.Reasoning != "" {
			item["reasoning_content"] = message.Reasoning
		}
		if message.ToolCallID != "" {
			item["tool_call_id"] = message.ToolCallID
		}
		if len(message.ToolCalls) > 0 {
			calls := make([]map[string]interface{}, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				calls = append(calls, map[string]interface{}{"id": call.ID, "type": "function", "function": map[string]interface{}{"name": call.Name, "arguments": string(call.Arguments)}})
			}
			item["tool_calls"] = calls
		}
		messages = append(messages, item)
	}
	payload := map[string]interface{}{"model": request.Model, "messages": messages, "stream": request.Stream}
	setChatOptions(payload, request)
	if request.Reasoning.Enabled {
		payload["thinking"] = map[string]interface{}{"type": "enabled"}
	}
	if len(request.Tools) > 0 {
		payload["tools"] = openAITools(request.Tools)
		if len(request.ToolChoice) > 0 {
			var name string
			if json.Unmarshal(request.ToolChoice, &name) == nil {
				payload["tool_choice"] = name
			} else {
				var choice struct {
					Name string `json:"name"`
				}
				if err := json.Unmarshal(request.ToolChoice, &choice); err != nil {
					return nil, errors.New("invalid toolChoice")
				}
				payload["tool_choice"] = map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": choice.Name}}
			}
		}
	}
	return json.Marshal(payload)
}

func (openAIChatAdapter) Response(raw []byte) (ChatResponse, error) {
	var payload struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      *struct {
				Content          json.RawMessage `json:"content"`
				Reasoning        string          `json:"reasoning"`
				ReasoningContent string          `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || len(payload.Choices) == 0 {
		return ChatResponse{}, errors.New("invalid OpenAI response")
	}
	choice := payload.Choices[0]
	if choice.Message == nil || choice.FinishReason == "" {
		return ChatResponse{}, errors.New("invalid OpenAI response: missing message or finish_reason")
	}
	message := ChatResponseMessage{Role: "assistant", Content: jsonText(choice.Message.Content), Reasoning: choice.Message.ReasoningContent}
	if message.Reasoning == "" {
		message.Reasoning = choice.Message.Reasoning
	}
	for _, rawCall := range choice.Message.ToolCalls {
		call := ChatToolCall{ID: rawCall.ID, Name: rawCall.Function.Name, Arguments: json.RawMessage(rawCall.Function.Arguments)}
		if err := validateToolCall(call); err != nil {
			return ChatResponse{}, fmt.Errorf("invalid OpenAI tool call: %w", err)
		}
		message.ToolCalls = append(message.ToolCalls, call)
	}
	return ChatResponse{Message: message, FinishReason: choice.FinishReason}, nil
}

func (openAIChatAdapter) Stream(reader io.Reader, emit func(canonicalDelta) error) error {
	type accumulator struct{ id, name, arguments string }
	calls := map[int]*accumulator{}
	finish := ""
	completed := false
	err := scanSSE(reader, func(data string) error {
		if data == "[DONE]" {
			completed = true
			return nil
		}
		var payload struct {
			Error   interface{} `json:"error"`
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Content          string `json:"content"`
					Reasoning        string `json:"reasoning"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int                              `json:"index"`
						ID       string                           `json:"id"`
						Function struct{ Name, Arguments string } `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			return errors.New("invalid OpenAI SSE event")
		}
		if payload.Error != nil {
			return fmt.Errorf("OpenAI stream error: %v", payload.Error)
		}
		if len(payload.Choices) == 0 {
			return nil
		}
		choice := payload.Choices[0]
		if choice.FinishReason != "" {
			finish = choice.FinishReason
			completed = true
		}
		reasoning := choice.Delta.ReasoningContent
		if reasoning == "" {
			reasoning = choice.Delta.Reasoning
		}
		if choice.Delta.Content != "" || reasoning != "" {
			if err := emit(canonicalDelta{Content: choice.Delta.Content, Reasoning: reasoning}); err != nil {
				return err
			}
		}
		for _, part := range choice.Delta.ToolCalls {
			call := calls[part.Index]
			if call == nil {
				call = &accumulator{}
				calls[part.Index] = call
			}
			if part.ID != "" {
				call.id = part.ID
			}
			if part.Function.Name != "" {
				call.name = part.Function.Name
			}
			call.arguments += part.Function.Arguments
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !completed {
		return errors.New("OpenAI stream ended before [DONE] or finish_reason")
	}
	toolCalls := make([]ChatToolCall, 0, len(calls))
	for _, i := range sortedMapKeys(calls) {
		call := calls[i]
		canonical := ChatToolCall{ID: call.id, Name: call.name, Arguments: json.RawMessage(call.arguments)}
		if err := validateToolCall(canonical); err != nil {
			return fmt.Errorf("invalid OpenAI streamed tool call: %w", err)
		}
		toolCalls = append(toolCalls, canonical)
	}
	return emit(canonicalDelta{ToolCalls: toolCalls, FinishReason: finish, Done: true})
}

type anthropicChatAdapter struct{}

func (anthropicChatAdapter) Request(request ChatRequest) ([]byte, error) {
	system := make([]string, 0)
	messages := make([]map[string]interface{}, 0, len(request.Messages))
	for _, message := range request.Messages {
		if message.Role == "system" {
			system = append(system, message.Text())
			continue
		}
		role := message.Role
		content := make([]map[string]interface{}, 0, 1+len(message.ToolCalls))
		if message.Role == "tool" {
			role = "user"
			content = append(content, map[string]interface{}{"type": "tool_result", "tool_use_id": message.ToolCallID, "content": message.Text()})
		} else {
			content = append(content, anthropicContent(message.Content)...)
			for _, call := range message.ToolCalls {
				var input map[string]interface{}
				_ = json.Unmarshal(call.Arguments, &input)
				content = append(content, map[string]interface{}{"type": "tool_use", "id": call.ID, "name": call.Name, "input": input})
			}
		}
		messages = append(messages, map[string]interface{}{"role": role, "content": content})
	}
	payload := map[string]interface{}{"model": request.Model, "messages": messages, "stream": request.Stream, "max_tokens": 4096}
	if len(system) > 0 {
		payload["system"] = strings.Join(system, "\n\n")
	}
	setChatOptions(payload, request)
	delete(payload, "presence_penalty")
	delete(payload, "frequency_penalty")
	delete(payload, "seed")
	delete(payload, "user")
	if len(request.Stop) > 0 {
		payload["stop_sequences"] = request.Stop
		delete(payload, "stop")
	}
	if request.Reasoning.Enabled {
		maxTokens, _ := payload["max_tokens"].(int)
		if maxTokens < 2 {
			return nil, errors.New("Anthropic reasoning requires maxTokens greater than one")
		}
		budget := 1024
		if request.Reasoning.BudgetTokens != nil {
			budget = *request.Reasoning.BudgetTokens
		}
		if budget >= maxTokens {
			budget = maxTokens - 1
		}
		payload["thinking"] = map[string]interface{}{"type": "enabled", "budget_tokens": budget}
	}
	if len(request.Tools) > 0 {
		tools := make([]map[string]interface{}, 0, len(request.Tools))
		for _, tool := range request.Tools {
			var schema map[string]interface{}
			_ = json.Unmarshal(tool.Parameters, &schema)
			tools = append(tools, map[string]interface{}{"name": tool.Name, "description": tool.Description, "input_schema": schema})
		}
		payload["tools"] = tools
		if len(request.ToolChoice) > 0 {
			choice, err := anthropicToolChoice(request.ToolChoice)
			if err != nil {
				return nil, err
			}
			payload["tool_choice"] = choice
		}
	}
	return json.Marshal(payload)
}

func (anthropicChatAdapter) Response(raw []byte) (ChatResponse, error) {
	var payload struct {
		StopReason string `json:"stop_reason"`
		Content    *[]struct {
			Type, Text, Thinking, ID, Name string
			Input                          json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ChatResponse{}, errors.New("invalid Anthropic response")
	}
	if payload.Content == nil || payload.StopReason == "" {
		return ChatResponse{}, errors.New("invalid Anthropic response: missing content or stop_reason")
	}
	message := ChatResponseMessage{Role: "assistant"}
	for _, block := range *payload.Content {
		switch block.Type {
		case "text":
			message.Content += block.Text
		case "thinking":
			message.Reasoning += block.Thinking
		case "tool_use":
			call := ChatToolCall{ID: block.ID, Name: block.Name, Arguments: block.Input}
			if err := validateToolCall(call); err != nil {
				return ChatResponse{}, fmt.Errorf("invalid Anthropic tool call: %w", err)
			}
			message.ToolCalls = append(message.ToolCalls, call)
		}
	}
	return ChatResponse{Message: message, FinishReason: payload.StopReason}, nil
}

func (anthropicChatAdapter) Stream(reader io.Reader, emit func(canonicalDelta) error) error {
	type toolState struct {
		id, name  string
		arguments strings.Builder
	}
	tools := map[int]*toolState{}
	finish := ""
	completed := false
	err := scanSSE(reader, func(data string) error {
		var event struct {
			Type         string                          `json:"type"`
			Index        int                             `json:"index"`
			ContentBlock struct{ Type, ID, Name string } `json:"content_block"`
			Delta        struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Error interface{} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return errors.New("invalid Anthropic SSE event")
		}
		switch event.Type {
		case "error":
			return fmt.Errorf("Anthropic stream error: %v", event.Error)
		case "content_block_start":
			if event.ContentBlock.Type == "tool_use" {
				tools[event.Index] = &toolState{id: event.ContentBlock.ID, name: event.ContentBlock.Name}
			}
		case "content_block_delta":
			switch event.Delta.Type {
			case "text_delta":
				return emit(canonicalDelta{Content: event.Delta.Text})
			case "thinking_delta":
				return emit(canonicalDelta{Reasoning: event.Delta.Thinking})
			case "input_json_delta":
				if tool := tools[event.Index]; tool != nil {
					tool.arguments.WriteString(event.Delta.PartialJSON)
				}
			}
		case "message_delta":
			finish = event.Delta.StopReason
		case "message_stop":
			completed = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !completed {
		return errors.New("Anthropic stream ended before message_stop")
	}
	calls := make([]ChatToolCall, 0, len(tools))
	for _, i := range sortedMapKeys(tools) {
		tool := tools[i]
		call := ChatToolCall{ID: tool.id, Name: tool.name, Arguments: json.RawMessage(tool.arguments.String())}
		if err := validateToolCall(call); err != nil {
			return fmt.Errorf("invalid Anthropic streamed tool call: %w", err)
		}
		calls = append(calls, call)
	}
	return emit(canonicalDelta{ToolCalls: calls, FinishReason: finish, Done: true})
}

type ollamaChatAdapter struct{}

func (ollamaChatAdapter) Request(request ChatRequest) ([]byte, error) {
	if request.Reasoning.BudgetTokens != nil {
		return nil, errors.New("Ollama adapter does not support reasoning budgetTokens")
	}
	if len(request.ToolChoice) > 0 && string(request.ToolChoice) != `"auto"` {
		return nil, errors.New("Ollama adapter only supports automatic tool choice")
	}
	messages := make([]map[string]interface{}, 0, len(request.Messages))
	for _, message := range request.Messages {
		text, images := ollamaContent(message.Content)
		item := map[string]interface{}{"role": message.Role, "content": text}
		if len(images) > 0 {
			item["images"] = images
		}
		if message.ToolCallID != "" {
			item["tool_call_id"] = message.ToolCallID
		}
		if len(message.ToolCalls) > 0 {
			calls := make([]map[string]interface{}, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				var arguments map[string]interface{}
				_ = json.Unmarshal(call.Arguments, &arguments)
				calls = append(calls, map[string]interface{}{"function": map[string]interface{}{"name": call.Name, "arguments": arguments}})
			}
			item["tool_calls"] = calls
		}
		messages = append(messages, item)
	}
	payload := map[string]interface{}{"model": request.Model, "messages": messages, "stream": request.Stream, "think": request.Reasoning.Enabled}
	options := map[string]interface{}{}
	if request.MaxTokens != nil {
		options["num_predict"] = *request.MaxTokens
	}
	if request.Temperature != nil {
		options["temperature"] = *request.Temperature
	}
	if request.TopP != nil {
		options["top_p"] = *request.TopP
	}
	if len(request.Stop) > 0 {
		options["stop"] = request.Stop
	}
	if len(options) > 0 {
		payload["options"] = options
	}
	if len(request.Tools) > 0 {
		payload["tools"] = openAITools(request.Tools)
	}
	return json.Marshal(payload)
}

func (ollamaChatAdapter) Response(raw []byte) (ChatResponse, error) {
	var payload struct {
		DoneReason string `json:"done_reason"`
		Done       bool   `json:"done"`
		Message    *struct {
			Content, Thinking string
			ToolCalls         []struct {
				Function struct {
					Name      string
					Arguments json.RawMessage
				}
			} `json:"tool_calls"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ChatResponse{}, errors.New("invalid Ollama response")
	}
	if payload.Message == nil || !payload.Done {
		return ChatResponse{}, errors.New("invalid Ollama response: missing message or done=true")
	}
	message := ChatResponseMessage{Role: "assistant", Content: payload.Message.Content, Reasoning: payload.Message.Thinking}
	for i, rawCall := range payload.Message.ToolCalls {
		call := ChatToolCall{ID: fmt.Sprintf("ollama-%d", i), Name: rawCall.Function.Name, Arguments: rawCall.Function.Arguments}
		if err := validateToolCall(call); err != nil {
			return ChatResponse{}, fmt.Errorf("invalid Ollama tool call: %w", err)
		}
		message.ToolCalls = append(message.ToolCalls, call)
	}
	return ChatResponse{Message: message, FinishReason: payload.DoneReason}, nil
}

func (ollamaChatAdapter) Stream(reader io.Reader, emit func(canonicalDelta) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxRelayUpstreamResponseBytes)
	var calls []ChatToolCall
	finish := ""
	completed := false
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var payload struct {
			Done       bool   `json:"done"`
			DoneReason string `json:"done_reason"`
			Error      string `json:"error"`
			Message    struct {
				Content, Thinking string
				ToolCalls         []struct {
					Function struct {
						Name      string
						Arguments json.RawMessage
					}
				} `json:"tool_calls"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &payload); err != nil {
			return errors.New("invalid Ollama stream event")
		}
		if payload.Error != "" {
			return errors.New(payload.Error)
		}
		if payload.Message.Content != "" || payload.Message.Thinking != "" {
			if err := emit(canonicalDelta{Content: payload.Message.Content, Reasoning: payload.Message.Thinking}); err != nil {
				return err
			}
		}
		for _, rawCall := range payload.Message.ToolCalls {
			call := ChatToolCall{ID: fmt.Sprintf("ollama-%d", len(calls)), Name: rawCall.Function.Name, Arguments: rawCall.Function.Arguments}
			if err := validateToolCall(call); err != nil {
				return fmt.Errorf("invalid Ollama streamed tool call: %w", err)
			}
			calls = append(calls, call)
		}
		if payload.Done {
			finish = payload.DoneReason
			completed = true
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !completed {
		return errors.New("Ollama stream ended before done=true")
	}
	return emit(canonicalDelta{ToolCalls: calls, FinishReason: finish, Done: true})
}

func setChatOptions(payload map[string]interface{}, request ChatRequest) {
	if request.MaxTokens != nil {
		payload["max_tokens"] = *request.MaxTokens
	}
	if request.Temperature != nil {
		payload["temperature"] = *request.Temperature
	}
	if request.TopP != nil {
		payload["top_p"] = *request.TopP
	}
	if request.PresencePenalty != nil {
		payload["presence_penalty"] = *request.PresencePenalty
	}
	if request.FrequencyPenalty != nil {
		payload["frequency_penalty"] = *request.FrequencyPenalty
	}
	if request.Seed != nil {
		payload["seed"] = *request.Seed
	}
	if len(request.Stop) > 0 {
		payload["stop"] = request.Stop
	}
	if request.User != nil {
		payload["user"] = *request.User
	}
}

func sortedMapKeys[T any](values map[int]T) []int {
	keys := make([]int, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	return keys
}

func openAITools(tools []ChatTool) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(tools))
	for _, tool := range tools {
		var parameters map[string]interface{}
		_ = json.Unmarshal(tool.Parameters, &parameters)
		result = append(result, map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": tool.Name, "description": tool.Description, "parameters": parameters}})
	}
	return result
}

func openAIContent(parts []ChatContentPart) []map[string]interface{} {
	content := make([]map[string]interface{}, 0, len(parts))
	for _, part := range parts {
		if part.Type == "text" {
			content = append(content, map[string]interface{}{"type": "text", "text": part.Text})
			continue
		}
		if strings.HasPrefix(part.File.MIMEType, "image/") {
			content = append(content, map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:" + part.File.MIMEType + ";base64," + part.File.DataBase64}})
		} else {
			content = append(content, map[string]interface{}{"type": "text", "text": fileText(*part.File)})
		}
	}
	return content
}

func anthropicContent(parts []ChatContentPart) []map[string]interface{} {
	content := make([]map[string]interface{}, 0, len(parts))
	for _, part := range parts {
		if part.Type == "text" {
			content = append(content, map[string]interface{}{"type": "text", "text": part.Text})
			continue
		}
		if strings.HasPrefix(part.File.MIMEType, "image/") {
			content = append(content, map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "base64", "media_type": part.File.MIMEType, "data": part.File.DataBase64}})
		} else {
			content = append(content, map[string]interface{}{"type": "text", "text": fileText(*part.File)})
		}
	}
	return content
}

func ollamaContent(parts []ChatContentPart) (string, []string) {
	text := make([]string, 0, len(parts))
	images := make([]string, 0)
	for _, part := range parts {
		if part.Type == "text" {
			if part.Text != "" {
				text = append(text, part.Text)
			}
		} else if strings.HasPrefix(part.File.MIMEType, "image/") {
			images = append(images, part.File.DataBase64)
		} else {
			text = append(text, fileText(*part.File))
		}
	}
	return strings.Join(text, "\n\n"), images
}

func anthropicToolChoice(raw json.RawMessage) (interface{}, error) {
	var name string
	if json.Unmarshal(raw, &name) == nil {
		switch name {
		case "auto":
			return map[string]interface{}{"type": "auto"}, nil
		case "any", "required":
			return map[string]interface{}{"type": "any"}, nil
		default:
			return nil, errors.New("invalid Anthropic toolChoice")
		}
	}
	var choice struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &choice) == nil && strings.TrimSpace(choice.Name) != "" {
		return map[string]interface{}{"type": "tool", "name": choice.Name}, nil
	}
	return nil, errors.New("invalid Anthropic toolChoice")
}

func jsonText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct{ Type, Text string }
	if json.Unmarshal(raw, &parts) == nil {
		var result strings.Builder
		for _, part := range parts {
			if part.Type == "text" {
				result.WriteString(part.Text)
			}
		}
		return result.String()
	}
	return ""
}

func scanSSE(reader io.Reader, consume func(string) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxRelayUpstreamResponseBytes)
	var data strings.Builder
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		value := strings.TrimSuffix(data.String(), "\n")
		data.Reset()
		return consume(value)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			data.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

func writeCanonicalSSE(c *gin.Context, adapter chatAdapter, body io.Reader) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	flusher, _ := c.Writer.(http.Flusher)
	emit := func(delta canonicalDelta) error {
		raw, err := json.Marshal(delta)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(c.Writer, "event: chunk\ndata: %s\n\n", raw); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	if err := adapter.Stream(body, emit); err != nil {
		errorType := "upstream_protocol_error"
		if errors.Is(err, ErrUpstreamTimeout) {
			errorType = "upstream_timeout"
		}
		c.Set("relayErrorType", errorType)
		_ = emit(canonicalDelta{Error: err.Error(), Done: true})
	}
}
