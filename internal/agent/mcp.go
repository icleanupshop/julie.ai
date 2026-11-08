package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

const maxMCPToolResultBytes = 256 << 10

var allowedMCPTools = map[string]struct{}{
	"list_files":       {},
	"read_file":        {},
	"search_knowledge": {},
}

type MCPToolProvider interface {
	Tools(context.Context) ([]Tool, error)
	Call(context.Context, FunctionCall) (string, error)
	Close() error
}

type MCPToolClient struct {
	mu       sync.Mutex
	endpoint string
	token    string
	client   *client.Client
}

func NewMCPToolClient(ctx context.Context, endpoint, token string) (*MCPToolClient, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return nil, fmt.Errorf("invalid MCP endpoint")
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("MCP bearer token is required")
	}
	remote := &MCPToolClient{endpoint: endpoint, token: token}
	if err := remote.reconnect(ctx); err != nil {
		return nil, err
	}
	return remote, nil
}

func (remote *MCPToolClient) Tools(ctx context.Context) ([]Tool, error) {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	result, err := remote.client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		if reconnectErr := remote.reconnect(ctx); reconnectErr != nil {
			return nil, fmt.Errorf("list MCP tools: %v; reconnect: %w", err, reconnectErr)
		}
		result, err = remote.client.ListTools(ctx, mcp.ListToolsRequest{})
	}
	if err != nil {
		return nil, err
	}
	tools := make([]Tool, 0, len(result.Tools))
	seen := make(map[string]bool, len(result.Tools))
	for _, remoteTool := range result.Tools {
		if _, ok := allowedMCPTools[remoteTool.Name]; !ok {
			continue
		}
		if seen[remoteTool.Name] {
			return nil, fmt.Errorf("MCP server returned duplicate tool %q", remoteTool.Name)
		}
		seen[remoteTool.Name] = true
		data, err := json.Marshal(remoteTool)
		if err != nil {
			return nil, fmt.Errorf("encode MCP tool schema: %w", err)
		}
		var definition struct {
			InputSchema map[string]any `json:"inputSchema"`
		}
		if err := json.Unmarshal(data, &definition); err != nil || definition.InputSchema == nil {
			return nil, fmt.Errorf("MCP tool %q has an invalid input schema", remoteTool.Name)
		}
		tools = append(tools, Tool{Type: "function", Function: ToolFunction{
			Name:        remoteTool.Name,
			Description: remoteTool.Description,
			Parameters:  definition.InputSchema,
		}})
	}
	return tools, nil
}

func (remote *MCPToolClient) Call(ctx context.Context, call FunctionCall) (string, error) {
	if _, ok := allowedMCPTools[call.Name]; !ok {
		return "", fmt.Errorf("MCP tool %q is not allowed", call.Name)
	}
	decoder := json.NewDecoder(strings.NewReader(call.Arguments))
	decoder.UseNumber()
	var arguments map[string]any
	if err := decoder.Decode(&arguments); err != nil || arguments == nil {
		return "", fmt.Errorf("invalid MCP tool arguments")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", fmt.Errorf("invalid MCP tool arguments")
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	request := mcp.CallToolRequest{Params: mcp.CallToolParams{Name: call.Name, Arguments: arguments}}
	result, err := remote.client.CallTool(ctx, request)
	if err != nil {
		if reconnectErr := remote.reconnect(ctx); reconnectErr != nil {
			return "", fmt.Errorf("call MCP tool: %v; reconnect: %w", err, reconnectErr)
		}
		result, err = remote.client.CallTool(ctx, request)
	}
	if err != nil {
		return "", fmt.Errorf("call MCP tool: %w", err)
	}
	parts := make([]string, 0, len(result.Content))
	for _, item := range result.Content {
		if text, ok := item.(mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	output := strings.Join(parts, "\n")
	if result.StructuredContent != nil {
		data, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return "", fmt.Errorf("encode MCP tool result: %w", err)
		}
		output = string(data)
	}
	if len(output) > maxMCPToolResultBytes {
		return "", fmt.Errorf("MCP tool result exceeds %d bytes", maxMCPToolResultBytes)
	}
	if result.IsError {
		return "", fmt.Errorf("%s", output)
	}
	return output, nil
}

func (remote *MCPToolClient) Close() error {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if remote.client == nil {
		return nil
	}
	return remote.client.Close()
}

func (remote *MCPToolClient) reconnect(ctx context.Context) error {
	if remote.client != nil {
		_ = remote.client.Close()
	}
	connected, err := client.NewStreamableHttpClient(remote.endpoint,
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + remote.token}),
		transport.WithHTTPTimeout(150*time.Second),
	)
	if err != nil {
		return fmt.Errorf("create MCP client: %w", err)
	}
	if err := connected.Start(ctx); err != nil {
		_ = connected.Close()
		return fmt.Errorf("start MCP client: %w", err)
	}
	_, err = connected.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{
		ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
		Capabilities:    mcp.ClientCapabilities{},
		ClientInfo:      mcp.Implementation{Name: "Julie", Version: "0.1.0"},
	}})
	if err != nil {
		_ = connected.Close()
		return fmt.Errorf("initialize MCP client: %w", err)
	}
	remote.client = connected
	return nil
}
