package agent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func NewMCPServer(root *os.Root, knowledge KnowledgeSearcher, remotes ...MCPToolProvider) (*mcpserver.MCPServer, error) {
	toolAgent := &Agent{Root: root, Knowledge: knowledge}
	server := mcpserver.NewMCPServer("Julie tools", "0.1.0",
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithRecovery(),
	)
	tools := make([]Tool, 0, 5)
	if root != nil {
		tools = append(tools, workspaceTools()...)
	}
	if knowledge != nil {
		tools = append(tools, knowledgeTools()...)
	}
	for _, definition := range tools {
		schema, err := json.Marshal(definition.Function.Parameters)
		if err != nil {
			panic(fmt.Errorf("encode tool schema %q: %w", definition.Function.Name, err))
		}
		tool := mcp.Tool{
			Name:           definition.Function.Name,
			Description:    definition.Function.Description,
			RawInputSchema: schema,
		}
		server.AddTool(tool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			arguments, err := json.Marshal(request.GetArguments())
			if err != nil {
				return mcp.NewToolResultError("invalid tool arguments"), nil
			}
			result, err := toolAgent.execute(ctx, FunctionCall{Name: request.Params.Name, Arguments: string(arguments)})
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if len(result) > maxMCPToolResultBytes {
				return mcp.NewToolResultError("tool result exceeds the size limit"), nil
			}
			return mcp.NewToolResultText(result), nil
		})
	}
	seen := make(map[string]bool)
	for _, definition := range tools {
		seen[definition.Function.Name] = true
	}
	for _, remote := range remotes {
		remoteTools, err := remote.Tools(context.Background())
		if err != nil {
			return nil, fmt.Errorf("discover remote MCP tools: %w", err)
		}
		for _, definition := range remoteTools {
			name := definition.Function.Name
			if _, ok := allowedMCPTools[name]; !ok {
				return nil, fmt.Errorf("remote MCP tool %q is not allowed", name)
			}
			if seen[name] {
				return nil, fmt.Errorf("duplicate MCP tool %q", name)
			}
			seen[name] = true
			schema, err := json.Marshal(definition.Function.Parameters)
			if err != nil {
				return nil, fmt.Errorf("encode tool schema %q: %w", name, err)
			}
			tool := mcp.Tool{Name: name, Description: definition.Function.Description, RawInputSchema: schema}
			provider := remote
			server.AddTool(tool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				arguments, err := json.Marshal(request.GetArguments())
				if err != nil {
					return mcp.NewToolResultError("invalid tool arguments"), nil
				}
				result, err := provider.Call(ctx, FunctionCall{Name: request.Params.Name, Arguments: string(arguments)})
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				if len(result) > maxMCPToolResultBytes {
					return mcp.NewToolResultError("tool result exceeds the size limit"), nil
				}
				return mcp.NewToolResultText(result), nil
			})
		}
	}
	return server, nil
}

func NewMCPHTTPHandler(mcpServer *mcpserver.MCPServer, token string) (http.Handler, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("MCP bearer token is required")
	}
	streamable := mcpserver.NewStreamableHTTPServer(mcpServer)
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		const prefix = "Bearer "
		header := request.Header.Get("Authorization")
		if !strings.HasPrefix(header, prefix) || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, prefix)), []byte(token)) != 1 {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		streamable.ServeHTTP(writer, request)
	}))
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	return mux, nil
}
