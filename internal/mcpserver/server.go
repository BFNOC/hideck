package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type tokenKey struct{}
type requestContextKey struct{}

func TokenID(ctx context.Context) string { id, _ := ctx.Value(tokenKey{}).(string); return id }

// A negative progress value reports another completed stage with an unknown total.
type Progress func(string, float64) error
type Result struct {
	Data      any
	Failed    bool
	Resources []*mcp.ResourceLink
}
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	ReadOnly    bool
	Destructive bool
	Idempotent  bool
	Execute     func(context.Context, json.RawMessage, Progress) (Result, error)
}
type Options struct {
	Tokens        *Tokens
	Enabled       func() bool
	Tools         []Tool
	ReadRecording func(context.Context, string) ([]byte, error)
	Logger        *slog.Logger
}

func New(options Options) (http.Handler, error) {
	server := mcp.NewServer(&mcp.Implementation{Name: "hideck", Version: "1.0.0"}, nil)
	for _, tool := range options.Tools {
		if err := register(server, tool, options); err != nil {
			return nil, err
		}
	}
	if options.ReadRecording != nil {
		server.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "hideck://recordings/{name}", Name: "通话录音", MIMEType: "audio/mpeg"}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			if err := authorizeCall(ctx, options); err != nil {
				return nil, err
			}
			data, err := options.ReadRecording(ctx, strings.TrimPrefix(req.Params.URI, "hideck://recordings/"))
			if err != nil {
				return nil, err
			}
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "audio/mpeg", Blob: data}}}, nil
		})
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, PropagateRequestCancellation: true, MaxRequestBodyBytes: -1})
	protected := http.NewCrossOriginProtection().Handler(handler)
	return authenticate(protected, options), nil
}

func authenticate(next http.Handler, options Options) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !options.Enabled() {
			http.Error(w, "MCP 服务已关闭", http.StatusServiceUnavailable)
			return
		}
		scheme, secret, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			rejectAuthentication(w)
			return
		}
		token, err := options.Tokens.Authenticate(r.Context(), secret)
		if errors.Is(err, ErrUnauthorized) {
			rejectAuthentication(w)
			return
		}
		if err != nil {
			http.Error(w, "MCP 令牌存储读取失败", http.StatusInternalServerError)
			return
		}
		ctx := context.WithValue(r.Context(), tokenKey{}, token.ID)
		ctx = context.WithValue(ctx, requestContextKey{}, r.Context())
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
func rejectAuthentication(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, ErrUnauthorized.Error(), http.StatusUnauthorized)
}
func authorizeCall(ctx context.Context, options Options) error {
	if !options.Enabled() {
		return errors.New("MCP 服务已关闭")
	}
	_, err := options.Tokens.Active(ctx, TokenID(ctx))
	return err
}

func register(server *mcp.Server, tool Tool, options Options) error {
	var schema jsonschema.Schema
	if err := json.Unmarshal(tool.Schema, &schema); err != nil {
		return fmt.Errorf("%s schema: %w", tool.Name, err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return fmt.Errorf("%s schema: %w", tool.Name, err)
	}
	server.AddTool(&mcp.Tool{Name: tool.Name, Description: tool.Description, InputSchema: &schema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: tool.ReadOnly, DestructiveHint: &tool.Destructive, IdempotentHint: tool.Idempotent}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			start := time.Now()
			result, err := invoke(ctx, req, tool, resolved, options)
			if options.Logger != nil {
				var args struct {
					DeviceID string `json:"device_id"`
					Request  struct {
						DeviceID string `json:"device_id"`
					} `json:"request"`
				}
				_ = json.Unmarshal(req.Params.Arguments, &args)
				if args.DeviceID == "" {
					args.DeviceID = args.Request.DeviceID
				}
				options.Logger.Info("MCP tool", "tool", tool.Name, "token_id", TokenID(ctx), "device_id", args.DeviceID, "duration_ms", time.Since(start).Milliseconds(), "failed", err != nil || result.IsError)
			}
			return result, err
		})
	return nil
}

func invoke(ctx context.Context, req *mcp.CallToolRequest, tool Tool, schema *jsonschema.Resolved, options Options) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if original, ok := ctx.Value(requestContextKey{}).(context.Context); ok {
		stop := context.AfterFunc(original, cancel)
		defer stop()
	}
	if err := authorizeCall(ctx, options); err != nil {
		return failure(err), nil
	}
	// MCP permits tools/call to omit arguments for tools without inputs.
	raw := req.Params.Arguments
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var arguments map[string]any
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return failure(err), nil
	}
	if err := schema.Validate(arguments); err != nil {
		return failure(fmt.Errorf("参数不符合工具契约: %w", err)), nil
	}
	var stages float64
	progress := func(message string, percent float64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		token := req.Params.GetProgressToken()
		if token == nil {
			return nil
		}
		total := float64(100)
		if percent < 0 {
			stages++
			percent = stages
			total = 0
		}
		return req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Message: message, Progress: percent, Total: total})
	}
	result, err := tool.Execute(ctx, raw, progress)
	if err != nil {
		return failure(err), nil
	}
	payload, err := json.Marshal(result.Data)
	if err != nil {
		return nil, err
	}
	response := &mcp.CallToolResult{StructuredContent: result.Data, IsError: result.Failed, Content: []mcp.Content{&mcp.TextContent{Text: string(payload)}}}
	for _, resource := range result.Resources {
		response.Content = append(response.Content, resource)
	}
	return response, nil
}
func failure(err error) *mcp.CallToolResult {
	data := map[string]any{"error": err.Error()}
	payload, _ := json.Marshal(data)
	return &mcp.CallToolResult{IsError: true, StructuredContent: data, Content: []mcp.Content{&mcp.TextContent{Text: string(payload)}}}
}
