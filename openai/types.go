package openai

// ChatCompletionMessage represents a single message in the chat.
type ChatCompletionMessage struct {
	// Role is the role of the author of this message (e.g., "user", "system", "assistant").
	Role string `json:"role"`
	// Content is the content of the message.
	Content string `json:"content"`
	// Name is an optional name for the participant, required for function calling.
	// Omitted here for simplicity in this core example.
	// Name string `json:"name,omitempty"`
}

// ChatCompletionRequest represents the request body for the Chat Completion API.
type ChatCompletionRequest struct {
	// Model is the model to use (e.g., "gpt-4", "gpt-3.5-turbo").
	Model string `json:"model"`
	// Messages is a list of messages comprising the conversation so far.
	Messages []ChatCompletionMessage `json:"messages"`
	// MaxTokens is the maximum number of tokens to generate in the chat completion.
	MaxTokens int `json:"max_tokens,omitempty"`
	// Temperature is the sampling temperature (0.0 to 2.0).
	Temperature float64 `json:"temperature,omitempty"`
	// TopP is an alternative to sampling with temperature.
	TopP float64 `json:"top_p,omitempty"`
	// N is how many chat completion choices to generate for each input message.
	N int `json:"n,omitempty"`
	// Stream is whether to stream back partial progress.
	Stream bool `json:"stream,omitempty"`
	// Stop is up to 4 sequences where the API will stop generating further tokens.
	Stop []string `json:"stop,omitempty"`
	// PresencePenalty is a number between -2.0 and 2.0.
	PresencePenalty float64 `json:"presence_penalty,omitempty"`
	// FrequencyPenalty is a number between -2.0 and 2.0.
	FrequencyPenalty float64 `json:"frequency_penalty,omitempty"`
	// LogitBias is a map that modifies the probability of specific tokens appearing in the completion.
	// LogitBias map[string]int `json:"logit_bias,omitempty"`
	// User is a unique identifier representing your end-user.
	User string `json:"user,omitempty"`
	// ResponseFormat specifies the format of the output, e.g., JSON mode.
	// ResponseFormat *ChatCompletionResponseFormat `json:"response_format,omitempty"`
}

// Usage represents the token usage in the response.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatCompletionChoice represents one of the multiple completion choices.
type ChatCompletionChoice struct {
	Index        int                   `json:"index"`
	Message      ChatCompletionMessage `json:"message"`       // Uses the same message struct as the request
	FinishReason string                `json:"finish_reason"` // e.g., "stop", "length", "tool_calls"
}

// ChatCompletionResponse represents the response body for the Chat Completion API.
type ChatCompletionResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"` // Should be "chat.completion"
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []ChatCompletionChoice `json:"choices"`
	Usage   Usage                  `json:"usage"`
	// SystemFingerprint string `json:"system_fingerprint"` // Optional, included in more recent structs
}

// APIErrorDetail represents the nested error structure in an OpenAI error response.
type APIErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    any    `json:"code,omitempty"` // Can be a string or null
}

// APIError represents the full error response body from the OpenAI API.
type APIError struct {
	Error APIErrorDetail `json:"error"`
}
