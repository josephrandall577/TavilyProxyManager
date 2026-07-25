package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"tavily-proxy/server/internal/services"
)

type Dependencies struct {
	MasterKey  *services.MasterKeyService
	Proxy      *services.TavilyProxy
	Stats      *services.StatsService
	Stateless  bool
	SessionTTL time.Duration
}

func NewHandler(deps Dependencies) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "tavily-proxy-mcp",
		Version: "0.1.0",
	}, nil)

	addProxyTool(server, deps.Proxy, &mcp.Tool{
		Name:        "tavily_search",
		Description: "Execute a search query using Tavily Search (via Tavily Proxy Pool). Returns ranked results and optional answer/raw_content/images/usage.",
		InputSchema: tavilySearchInputSchema,
	}, http.MethodPost, "/search")
	addProxyTool(server, deps.Proxy, &mcp.Tool{
		Name:        "tavily_extract",
		Description: "Extract structured content from URLs (via Tavily Proxy Pool)",
		InputSchema: tavilyExtractInputSchema,
	}, http.MethodPost, "/extract")
	addProxyTool(server, deps.Proxy, &mcp.Tool{
		Name:        "tavily_crawl",
		Description: "Crawl a website starting from a root URL (via Tavily Proxy Pool)",
		InputSchema: tavilyCrawlInputSchema,
	}, http.MethodPost, "/crawl")
	addProxyTool(server, deps.Proxy, &mcp.Tool{
		Name:        "tavily_map",
		Description: "Map a website's URL structure (via Tavily Proxy Pool)",
		InputSchema: tavilyMapInputSchema,
	}, http.MethodPost, "/map")
	addResearchTool(server, deps.Proxy)
	addUsageTool(server, deps.Stats, &mcp.Tool{
		Name:        "tavily_usage",
		Description: "Get aggregated usage/quota info from local key statistics",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
	})

	base := mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{
		Stateless:      deps.Stateless,
		SessionTimeout: deps.SessionTTL,
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := parseBearerToken(r.Header.Get("Authorization"))
		if !deps.MasterKey.Authenticate(token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		base.ServeHTTP(w, r)
	})
}

func addProxyTool(server *mcp.Server, proxy *services.TavilyProxy, tool *mcp.Tool, method, path string) {
	server.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var body []byte
		if method == http.MethodPost {
			if len(req.Params.Arguments) > 0 {
				body = req.Params.Arguments
			} else {
				body = []byte("{}")
			}
		}

		resp, err := doProxyRequest(ctx, proxy, method, path, body, nil, 0, 0)
		return proxyToolResult(resp, err), nil
	})
}

func addResearchTool(server *mcp.Server, proxy *services.TavilyProxy) {
	server.AddTool(&mcp.Tool{
		Name:        "tavily_research",
		Description: "Perform comprehensive research on a given topic or question. Returns the completed report.",
		InputSchema: tavilyResearchInputSchema,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Input string `json:"input"`
			Model string `json:"model"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil || strings.TrimSpace(args.Input) == "" {
			return researchToolResult("", errors.New("input is required")), nil
		}
		if args.Model == "" {
			args.Model = "auto"
		}
		content, err := runResearch(ctx, proxy, args.Input, args.Model)
		return researchToolResult(content, err), nil
	})
}

func runResearch(ctx context.Context, proxy *services.TavilyProxy, input, model string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, researchMaxDuration(model))
	defer cancel()

	body, err := json.Marshal(map[string]any{"input": input, "model": model})
	if err != nil {
		return "", err
	}
	resp, err := doProxyRequest(ctx, proxy, http.MethodPost, "/research", body, nil, 0, 0)
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusBadRequest && researchStreamRequired(resp.Body) {
		return runResearchStream(ctx, proxy, input, model, resp.KeyID)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(resp.Body)))
	}

	var created struct {
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(resp.Body, &created) != nil || created.RequestID == "" {
		return "", errors.New("no request_id returned from research endpoint")
	}

	pollInterval := 2 * time.Second
	for {
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}

		statusResp, err := doProxyRequest(ctx, proxy, http.MethodGet, "/research/"+url.PathEscape(created.RequestID), nil, nil, 0, resp.KeyID)
		if err != nil {
			return "", err
		}
		if statusResp.StatusCode == http.StatusNotFound {
			return "", errors.New("research task not found")
		}
		if statusResp.StatusCode < http.StatusOK || statusResp.StatusCode >= http.StatusMultipleChoices {
			return "", fmt.Errorf("upstream status %d: %s", statusResp.StatusCode, strings.TrimSpace(string(statusResp.Body)))
		}
		var status struct {
			Status  string `json:"status"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(statusResp.Body, &status); err != nil {
			return "", err
		}
		switch status.Status {
		case "completed":
			return status.Content, nil
		case "failed":
			return "", errors.New("research task failed")
		}
		pollInterval = min(pollInterval+pollInterval/2, 10*time.Second)
	}
}

func researchStreamRequired(body []byte) bool {
	var response struct {
		Detail struct {
			ErrorCode string `json:"error_code"`
		} `json:"detail"`
	}
	return json.Unmarshal(body, &response) == nil && response.Detail.ErrorCode == "research_stream_required"
}

func researchMaxDuration(model string) time.Duration {
	if model == "mini" {
		return 5 * time.Minute
	}
	return 15 * time.Minute
}

func runResearchStream(ctx context.Context, proxy *services.TavilyProxy, input, model string, keyID uint) (string, error) {
	body, err := json.Marshal(map[string]any{"input": input, "model": model, "stream": true})
	if err != nil {
		return "", err
	}
	var content string
	resp, err := doProxyRequest(ctx, proxy, http.MethodPost, "/research", body, func(stream services.ProxyStreamResponse) error {
		content, err = readResearchStream(stream.Body)
		return err
	}, researchMaxDuration(model), keyID)
	if err != nil {
		return "", err
	}
	if !resp.Streamed {
		return "", fmt.Errorf("upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(resp.Body)))
	}
	return content, nil
}

func readResearchStream(body io.Reader) (string, error) {
	reader := bufio.NewReader(body)
	var content strings.Builder
	eventType := "message"
	var dataLines []string

	flush := func() (bool, error) {
		data := strings.Join(dataLines, "\n")
		defer func() {
			eventType = "message"
			dataLines = dataLines[:0]
		}()
		switch eventType {
		case "done":
			if content.Len() == 0 {
				return false, errors.New("research stream completed without content")
			}
			return true, nil
		case "error":
			var event struct {
				Error any `json:"error"`
			}
			if json.Unmarshal([]byte(data), &event) == nil && event.Error != nil {
				return false, fmt.Errorf("research stream error: %v", event.Error)
			}
			return false, fmt.Errorf("research stream error: %s", data)
		}
		var event struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &event) == nil && len(event.Choices) > 0 {
			content.WriteString(event.Choices[0].Delta.Content)
		}
		return false, nil
	}

	for {
		line, readErr := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			done, err := flush()
			if err != nil {
				return "", err
			}
			if done {
				return content.String(), nil
			}
		} else if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}

		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return "", readErr
			}
			if eventType != "message" || len(dataLines) > 0 {
				done, err := flush()
				if err != nil {
					return "", err
				}
				if done {
					return content.String(), nil
				}
			}
			return "", errors.New("research stream ended before completion")
		}
	}
}

func doProxyRequest(ctx context.Context, proxy *services.TavilyProxy, method, path string, body []byte, stream func(services.ProxyStreamResponse) error, timeout time.Duration, preferredKeyID uint) (services.ProxyResponse, error) {
	headers := http.Header{"User-Agent": {"tavily-proxy-mcp"}}
	if method == http.MethodPost {
		headers.Set("Content-Type", "application/json")
	}
	return proxy.Do(ctx, services.ProxyRequest{
		Method: method, Path: path, Headers: headers, Body: body,
		ClientIP: "mcp", ContentType: "application/json", Stream: stream, Timeout: timeout, PreferredKeyID: preferredKeyID,
	})
}

func researchToolResult(content string, err error) *mcp.CallToolResult {
	if err != nil {
		content = "Research Error: " + err.Error()
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: content}}}
}

func proxyToolResult(resp services.ProxyResponse, err error) *mcp.CallToolResult {
	if err != nil {
		return &mcp.CallToolResult{
			IsError:           true,
			Content:           []mcp.Content{&mcp.TextContent{Text: err.Error()}},
			StructuredContent: map[string]any{"error": err.Error()},
		}
	}

	text := string(resp.Body)
	var parsed any
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		parsed = nil
	}
	structured, ok := parsed.(map[string]any)
	if !ok {
		structured = map[string]any{"raw": text}
	}
	result := &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: text}},
		StructuredContent: structured,
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.IsError = true
		result.Content = []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Upstream status %d: %s", resp.StatusCode, text)}}
	}
	return result
}

func addUsageTool(server *mcp.Server, stats *services.StatsService, tool *mcp.Tool) {
	server.AddTool(tool, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if stats == nil {
			const msg = "stats service unavailable"
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{
					&mcp.TextContent{Text: msg},
				},
				StructuredContent: map[string]any{"error": msg},
			}, nil
		}

		s, err := stats.Get(ctx)
		if err != nil {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{
					&mcp.TextContent{Text: err.Error()},
				},
				StructuredContent: map[string]any{"error": err.Error()},
			}, nil
		}

		payload := map[string]any{
			"key": map[string]any{
				"usage": s.TotalUsed,
				"limit": s.TotalQuota,
			},
		}

		raw, err := json.Marshal(payload)
		if err != nil {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{
					&mcp.TextContent{Text: err.Error()},
				},
				StructuredContent: map[string]any{"error": err.Error()},
			}, nil
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: string(raw)},
			},
			StructuredContent: payload,
		}, nil
	})
}

var tavilySearchInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": true,
	"required":             []string{"query"},
	"properties": map[string]any{
		"query": map[string]any{
			"type":        "string",
			"description": "The search query to execute with Tavily.",
		},
		"auto_parameters": map[string]any{
			"type":        "boolean",
			"default":     false,
			"description": "Automatically configures search parameters based on the query. Explicit values override auto-selected ones. Note: include_answer/include_raw_content/max_results must be set manually. auto_parameters may set search_depth=advanced (2 credits); set search_depth=basic to avoid extra cost.",
		},
		"topic": map[string]any{
			"type":        "string",
			"enum":        []string{"general", "news", "finance"},
			"default":     "general",
			"description": "Search topic/category. Use news for real-time updates; general for broad searches.",
		},
		"search_depth": map[string]any{
			"type":        "string",
			"enum":        []string{"advanced", "basic", "fast", "ultra-fast"},
			"default":     "basic",
			"description": "Controls relevance vs latency and how results[].content is generated. basic: balanced, 1 summary per URL (1 credit). fast: lower latency, multiple snippets per URL (1 credit). ultra-fast: lowest latency, 1 summary per URL (1 credit). advanced: highest relevance, multiple snippets per URL (2 credits).",
		},
		"chunks_per_source": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"maximum":     3,
			"default":     3,
			"description": "Max number of relevant chunks (each up to ~500 chars) to return per source. Used with search_depth=advanced.",
		},
		"max_results": map[string]any{
			"type":        "integer",
			"minimum":     0,
			"maximum":     20,
			"default":     5,
			"description": "The maximum number of search results to return.",
		},
		"time_range": map[string]any{
			"type":        "string",
			"enum":        []string{"day", "week", "month", "year", "d", "w", "m", "y"},
			"default":     nil,
			"description": "Filter results by publish/updated time window back from now (day/week/month/year or d/w/m/y).",
		},
		"start_date": map[string]any{
			"type":        "string",
			"format":      "date",
			"default":     nil,
			"description": "Return results after this date (YYYY-MM-DD).",
		},
		"end_date": map[string]any{
			"type":        "string",
			"format":      "date",
			"default":     nil,
			"description": "Return results before this date (YYYY-MM-DD).",
		},
		"country": map[string]any{
			"type":        "string",
			"default":     nil,
			"description": "Boost results from a specific country (topic=general only). Use lowercase country names like 'united states'.",
		},
		"include_domains": map[string]any{
			"type":        "array",
			"default":     []any{},
			"items":       map[string]any{"type": "string"},
			"description": "A list of domains to specifically include in the search results (max 300).",
		},
		"exclude_domains": map[string]any{
			"type":        "array",
			"default":     []any{},
			"items":       map[string]any{"type": "string"},
			"description": "A list of domains to specifically exclude from the search results (max 150).",
		},
		"include_images": map[string]any{
			"type":        "boolean",
			"default":     false,
			"description": "Also perform an image search and include images in the response.",
		},
		"include_image_descriptions": map[string]any{
			"type":        "boolean",
			"default":     false,
			"description": "When include_images is true, also add a descriptive text for each image.",
		},
		"include_answer": map[string]any{
			"description": "Include an LLM-generated answer to the query. true/basic: quick answer; advanced: more detailed.",
			"oneOf": []any{
				map[string]any{"type": "boolean"},
				map[string]any{"type": "string", "enum": []string{"basic", "advanced"}},
			},
			"default": false,
		},
		"include_raw_content": map[string]any{
			"description": "Include cleaned/parsed page content for each result. true/markdown: markdown; text: plain text (may increase latency).",
			"oneOf": []any{
				map[string]any{"type": "boolean"},
				map[string]any{"type": "string", "enum": []string{"markdown", "text"}},
			},
			"default": false,
		},
		"include_favicon": map[string]any{
			"type":        "boolean",
			"default":     false,
			"description": "Whether to include the favicon URL for each result.",
		},
		"include_usage": map[string]any{
			"type":        "boolean",
			"default":     false,
			"description": "Whether to include credit usage information in the response.",
		},
		"exact_match": map[string]any{
			"type":        "boolean",
			"default":     false,
			"description": "Only return results containing exact quoted phrases from the query.",
		},
		"timeout": map[string]any{
			"type":        "number",
			"minimum":     1,
			"default":     60,
			"description": "Request timeout in seconds.",
		},
		"safe_search": map[string]any{
			"type":        "boolean",
			"default":     false,
			"description": "Enable Enterprise safe-search filtering; unsupported with fast and ultra-fast depth.",
		},
	},
}

var tavilyExtractInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": true,
	"required":             []string{"urls"},
	"properties": map[string]any{
		"urls": map[string]any{
			"oneOf": []any{
				map[string]any{"type": "string"},
				map[string]any{"type": "array", "maxItems": 20, "items": map[string]any{"type": "string"}},
			},
			"description": "URLs to extract content from.",
		},
		"query": map[string]any{
			"type":        "string",
			"description": "Query used to rank relevant chunks from each URL.",
		},
		"chunks_per_source": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"maximum":     5,
			"description": "Relevant chunks per source when query is supplied.",
		},
		"extract_depth": map[string]any{
			"type":        "string",
			"enum":        []string{"basic", "advanced"},
			"default":     "basic",
			"description": "Depth of extraction.",
		},
		"format": map[string]any{
			"type":        "string",
			"enum":        []string{"markdown", "text"},
			"default":     "markdown",
			"description": "Output format for extracted content.",
		},
		"include_images": map[string]any{
			"type":        "boolean",
			"default":     false,
			"description": "Include images.",
		},
		"include_favicon": map[string]any{
			"type":        "boolean",
			"default":     false,
			"description": "Include favicon URL.",
		},
		"include_usage": map[string]any{
			"type":        "boolean",
			"default":     false,
			"description": "Include credit usage information in the response.",
		},
		"timeout": map[string]any{
			"type":        "number",
			"minimum":     1,
			"maximum":     60,
			"default":     30,
			"description": "Request timeout in seconds.",
		},
	},
}

var tavilyMapInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": true,
	"required":             []string{"url"},
	"properties": map[string]any{
		"url": map[string]any{
			"type":        "string",
			"description": "Root URL to begin mapping.",
		},
		"instructions": map[string]any{
			"type":        "string",
			"description": "Natural language instructions for the crawler.",
		},
		"max_depth": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"maximum":     5,
			"default":     1,
			"description": "Max depth of mapping.",
		},
		"max_breadth": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"maximum":     500,
			"default":     20,
			"description": "Max number of links to follow per level.",
		},
		"limit": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"default":     50,
			"description": "Total number of links to process.",
		},
		"select_paths": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "Regex patterns to include specific paths.",
		},
		"select_domains": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "Regex patterns to include specific domains.",
		},
		"exclude_paths": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "Regex patterns to exclude paths.",
		},
		"exclude_domains": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "Regex patterns to exclude domains.",
		},
		"allow_external": map[string]any{
			"type":        "boolean",
			"default":     true,
			"description": "Allow following external-domain links.",
		},
		"include_usage": map[string]any{
			"type":        "boolean",
			"default":     false,
			"description": "Include credit usage information in the response.",
		},
		"timeout": map[string]any{
			"type":        "number",
			"minimum":     1,
			"default":     150,
			"description": "Request timeout in seconds.",
		},
	},
}

var tavilyCrawlInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": true,
	"required":             []string{"url"},
	"properties": map[string]any{
		"url": map[string]any{
			"type":        "string",
			"description": "Root URL to begin crawling.",
		},
		"instructions": map[string]any{
			"type":        "string",
			"description": "Natural language instructions for the crawler.",
		},
		"chunks_per_source": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"maximum":     5,
			"description": "Relevant chunks per page when instructions are supplied.",
		},
		"max_depth": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"maximum":     5,
			"default":     1,
			"description": "Max depth of crawl.",
		},
		"max_breadth": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"maximum":     500,
			"default":     20,
			"description": "Max number of links to follow per level.",
		},
		"limit": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"default":     50,
			"description": "Total number of pages to process.",
		},
		"select_paths": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "Regex patterns to include specific paths.",
		},
		"select_domains": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "Regex patterns to include specific domains.",
		},
		"exclude_paths": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "Regex patterns to exclude paths.",
		},
		"exclude_domains": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "Regex patterns to exclude domains.",
		},
		"allow_external": map[string]any{
			"type":        "boolean",
			"default":     true,
			"description": "Allow following external-domain links.",
		},
		"include_images": map[string]any{
			"type":        "boolean",
			"default":     false,
			"description": "Include images discovered during crawling.",
		},
		"extract_depth": map[string]any{
			"type":        "string",
			"enum":        []string{"basic", "advanced"},
			"default":     "basic",
			"description": "Extraction depth for crawled pages.",
		},
		"format": map[string]any{
			"type":        "string",
			"enum":        []string{"markdown", "text"},
			"default":     "markdown",
			"description": "Format of extracted content.",
		},
		"include_favicon": map[string]any{
			"type":        "boolean",
			"default":     false,
			"description": "Include favicon URL for each result.",
		},
		"include_usage": map[string]any{
			"type":        "boolean",
			"default":     false,
			"description": "Include credit usage information in the response.",
		},
		"timeout": map[string]any{
			"type":        "number",
			"minimum":     1,
			"default":     150,
			"description": "Request timeout in seconds.",
		},
	},
}

var tavilyResearchInputSchema = map[string]any{
	"type":     "object",
	"required": []string{"input"},
	"properties": map[string]any{
		"input": map[string]any{"type": "string", "description": "Research task or question."},
		"model": map[string]any{
			"type": "string", "enum": []string{"mini", "pro", "auto"}, "default": "auto",
		},
	},
}

func parseBearerToken(authHeader string) string {
	if authHeader == "" {
		return ""
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 {
		return ""
	}
	if !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}
