package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Model struct {
	ID          string `json:"id"`
	URL         string `json:"url"`
	APIType     string `json:"apiType"`
	ToolCalling bool   `json:"toolCalling"`
}

type Config struct {
	APIKey  string  `json:"apiKey"`
	APIType string  `json:"apiType"`
	Models  []Model `json:"models"`
}

func LoadConfig(path, modelID string) (Config, Model, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, Model{}, fmt.Errorf("read model config: %w", err)
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return Config{}, Model{}, fmt.Errorf("decode model config: %w", err)
	}
	if key, ok := os.LookupEnv("JULIE_API_KEY"); ok {
		config.APIKey = key
	}
	for _, model := range config.Models {
		if modelID != "" && model.ID != modelID {
			continue
		}
		apiType := model.APIType
		if apiType == "" {
			apiType = config.APIType
		}
		if apiType != "chat-completions" {
			return Config{}, Model{}, fmt.Errorf("unsupported API type %q", apiType)
		}
		endpoint, err := url.Parse(model.URL)
		if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.User != nil {
			return Config{}, Model{}, fmt.Errorf("invalid model endpoint")
		}
		if model.ID == "" {
			return Config{}, Model{}, fmt.Errorf("model ID must not be empty")
		}
		return config, model, nil
	}
	return Config{}, Model{}, fmt.Errorf("no matching model in config (requested %q)", modelID)
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type completionRequest struct {
	Model      string    `json:"model"`
	Messages   []Message `json:"messages"`
	Tools      []Tool    `json:"tools,omitempty"`
	ToolChoice string    `json:"tool_choice,omitempty"`
	Stream     bool      `json:"stream"`
}

type Client struct {
	Model  Model
	APIKey string
	HTTP   *http.Client
}

func NewClient(config Config, model Model) *Client {
	return &Client{Model: model, APIKey: config.APIKey, HTTP: &http.Client{
		Timeout: 120 * time.Second,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (client *Client) Complete(ctx context.Context, messages []Message, tools []Tool) (Message, error) {
	payload := completionRequest{Model: client.Model.ID, Messages: messages}
	if client.Model.ToolCalling && len(tools) > 0 {
		payload.Tools = tools
		payload.ToolChoice = "auto"
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return Message{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.Model.URL, bytes.NewReader(data))
	if err != nil {
		return Message{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	if client.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+client.APIKey)
	}
	response, err := client.HTTP.Do(request)
	if err != nil {
		return Message{}, fmt.Errorf("call model: %w", err)
	}
	defer response.Body.Close()
	const maxResponseBytes = 4 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return Message{}, fmt.Errorf("read model response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return Message{}, fmt.Errorf("model response exceeds 4 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Message{}, fmt.Errorf("model returned HTTP %d", response.StatusCode)
	}
	var result struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return Message{}, fmt.Errorf("decode model response: %w", err)
	}
	if len(result.Choices) == 0 {
		return Message{}, fmt.Errorf("model returned no choices")
	}
	message := result.Choices[0].Message
	if message.Role != "assistant" || (strings.TrimSpace(message.Content) == "" && len(message.ToolCalls) == 0) {
		return Message{}, fmt.Errorf("model returned an invalid assistant message")
	}
	return message, nil
}
