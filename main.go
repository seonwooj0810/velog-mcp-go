package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const defaultEndpoint = "https://v3.velog.io/graphql"

var (
	endpoint  = getenv("VELOG_ENDPOINT", defaultEndpoint)
	tokenFile = os.Getenv("VELOG_TOKEN_FILE")

	tokenMu      sync.Mutex
	accessToken  = os.Getenv("VELOG_ACCESS_TOKEN")
	refreshToken = os.Getenv("VELOG_REFRESH_TOKEN")

	httpClient = &http.Client{}
)

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type savedTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func loadTokenFile() {
	if tokenFile == "" {
		return
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "[velog-mcp] failed to read VELOG_TOKEN_FILE: %v\n", err)
		}
		return
	}
	var saved savedTokens
	if err := json.Unmarshal(raw, &saved); err != nil {
		fmt.Fprintf(os.Stderr, "[velog-mcp] failed to parse VELOG_TOKEN_FILE: %v\n", err)
		return
	}
	if saved.AccessToken != "" {
		accessToken = saved.AccessToken
	}
	if saved.RefreshToken != "" {
		refreshToken = saved.RefreshToken
	}
}

// persistTokens writes the current in-memory tokens to disk. Caller must hold tokenMu.
func persistTokens() {
	if tokenFile == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(tokenFile), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "[velog-mcp] failed to mkdir for VELOG_TOKEN_FILE: %v\n", err)
		return
	}
	data, err := json.MarshalIndent(savedTokens{AccessToken: accessToken, RefreshToken: refreshToken}, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[velog-mcp] failed to marshal tokens: %v\n", err)
		return
	}
	if err := os.WriteFile(tokenFile, data, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "[velog-mcp] failed to persist tokens: %v\n", err)
	}
}

// absorbRefreshedTokens scans Set-Cookie headers for refreshed access/refresh tokens.
func absorbRefreshedTokens(resp *http.Response) {
	cookies := resp.Cookies()
	if len(cookies) == 0 {
		return
	}
	tokenMu.Lock()
	defer tokenMu.Unlock()
	changed := false
	for _, c := range cookies {
		if c.Name != "access_token" && c.Name != "refresh_token" {
			continue
		}
		if c.Value == "" {
			// Server cleared the cookie (e.g. refresh_token invalid → logged out).
			// Don't clobber memory, but surface it so the user knows why subsequent calls fail.
			fmt.Fprintf(os.Stderr, "[velog-mcp] server cleared %s cookie — token likely invalid/expired\n", c.Name)
			continue
		}
		switch c.Name {
		case "access_token":
			if c.Value != accessToken {
				accessToken = c.Value
				changed = true
			}
		case "refresh_token":
			if c.Value != refreshToken {
				refreshToken = c.Value
				changed = true
			}
		}
	}
	if changed {
		fmt.Fprintln(os.Stderr, "[velog-mcp] tokens refreshed by server")
		persistTokens()
	}
}

type gqlResponse struct {
	Data   json.RawMessage `json:"data,omitempty"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors,omitempty"`
}

func velogRequest(ctx context.Context, query string, variables map[string]any, requireAuth bool) (json.RawMessage, error) {
	tokenMu.Lock()
	at, rt := accessToken, refreshToken
	tokenMu.Unlock()

	if requireAuth && at == "" && rt == "" {
		return nil, errors.New("VELOG_ACCESS_TOKEN (or VELOG_REFRESH_TOKEN) is not set. Add it to the MCP server env to use authenticated tools.")
	}

	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if at != "" {
		req.AddCookie(&http.Cookie{Name: "access_token", Value: at})
	}
	if rt != "" {
		req.AddCookie(&http.Cookie{Name: "refresh_token", Value: rt})
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	absorbRefreshedTokens(resp)

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("Velog HTTP %d: %s", resp.StatusCode, string(raw))
	}

	var parsed gqlResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(parsed.Errors) > 0 {
		msgs := make([]string, 0, len(parsed.Errors))
		for _, e := range parsed.Errors {
			msgs = append(msgs, e.Message)
		}
		return nil, fmt.Errorf("Velog GraphQL error: %s", joinSemicolon(msgs))
	}
	if len(parsed.Data) == 0 || string(parsed.Data) == "null" {
		return nil, errors.New("Velog GraphQL: empty data")
	}
	return parsed.Data, nil
}

func joinSemicolon(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}

// jsonText pretty-prints a JSON value and wraps it in an MCP text result.
func jsonText(v any) (*mcp.CallToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(data)}},
	}, nil
}

// rawJSONText returns the raw JSON bytes as a pretty-printed text result.
func rawJSONText(raw json.RawMessage) (*mcp.CallToolResult, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return jsonText(v)
}

// ---------- Tool input types ----------

type emptyInput struct{}

type listPostsInput struct {
	Username string `json:"username,omitempty" jsonschema:"Velog username (without @). Omit for global feed."`
	Tag      string `json:"tag,omitempty"`
	Cursor   string `json:"cursor,omitempty" jsonschema:"Post id to paginate after."`
	Limit    int    `json:"limit,omitempty"`
	TempOnly bool   `json:"temp_only,omitempty" jsonschema:"Only the authenticated user's draft posts."`
}

type getPostInput struct {
	ID       string `json:"id,omitempty"`
	Username string `json:"username,omitempty"`
	URLSlug  string `json:"url_slug,omitempty"`
}

type writePostInput struct {
	Title      string         `json:"title"`
	Body       string         `json:"body" jsonschema:"Post body in Markdown."`
	Tags       []string       `json:"tags,omitempty"`
	IsPrivate  bool           `json:"is_private,omitempty"`
	IsTemp     bool           `json:"is_temp,omitempty" jsonschema:"Save as draft instead of publishing."`
	URLSlug    string         `json:"url_slug,omitempty" jsonschema:"Custom slug. Leave empty to derive from title."`
	Thumbnail  string         `json:"thumbnail,omitempty"`
	SeriesID   string         `json:"series_id,omitempty"`
	Meta       map[string]any `json:"meta,omitempty"`
}

type editPostInput struct {
	ID string `json:"id"`
	writePostInput
}

// writePostPayload mirrors the GraphQL input shape (always includes the defaulted
// fields and the is_markdown flag, like the TS version).
type writePostPayload struct {
	ID         string         `json:"id,omitempty"`
	Title      string         `json:"title"`
	Body       string         `json:"body"`
	Tags       []string       `json:"tags"`
	IsPrivate  bool           `json:"is_private"`
	IsTemp     bool           `json:"is_temp"`
	IsMarkdown bool           `json:"is_markdown"`
	URLSlug    string         `json:"url_slug"`
	Thumbnail  *string        `json:"thumbnail,omitempty"`
	SeriesID   *string        `json:"series_id,omitempty"`
	Meta       map[string]any `json:"meta"`
}

func buildWritePayload(in writePostInput, id string) writePostPayload {
	p := writePostPayload{
		ID:         id,
		Title:      in.Title,
		Body:       in.Body,
		Tags:       in.Tags,
		IsPrivate:  in.IsPrivate,
		IsTemp:     in.IsTemp,
		IsMarkdown: true,
		URLSlug:    in.URLSlug,
		Meta:       in.Meta,
	}
	if p.Tags == nil {
		p.Tags = []string{}
	}
	if p.Meta == nil {
		p.Meta = map[string]any{}
	}
	if in.Thumbnail != "" {
		t := in.Thumbnail
		p.Thumbnail = &t
	}
	if in.SeriesID != "" {
		s := in.SeriesID
		p.SeriesID = &s
	}
	return p
}

// ---------- Tool handlers ----------

func whoamiHandler(ctx context.Context, req *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
	data, err := velogRequest(ctx,
		`query { currentUser { id username email profile { display_name short_bio thumbnail } } }`,
		map[string]any{}, true)
	if err != nil {
		return nil, nil, err
	}
	var wrap struct {
		CurrentUser json.RawMessage `json:"currentUser"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		return nil, nil, err
	}
	res, err := rawJSONText(wrap.CurrentUser)
	return res, nil, err
}

func listPostsHandler(ctx context.Context, req *mcp.CallToolRequest, in listPostsInput) (*mcp.CallToolResult, any, error) {
	if in.Limit == 0 {
		in.Limit = 20
	}
	if in.Limit < 1 || in.Limit > 50 {
		return nil, nil, errors.New("limit must be between 1 and 50")
	}

	// Build input map with only the fields that were provided (mirrors the TS
	// behaviour where omitted optional args aren't sent to the server).
	input := map[string]any{"limit": in.Limit}
	if in.Username != "" {
		input["username"] = in.Username
	}
	if in.Tag != "" {
		input["tag"] = in.Tag
	}
	if in.Cursor != "" {
		input["cursor"] = in.Cursor
	}
	if in.TempOnly {
		input["temp_only"] = true
	}

	data, err := velogRequest(ctx,
		`query Posts($input: GetPostsInput!) {
		   posts(input: $input) {
		     id title url_slug short_description thumbnail
		     released_at updated_at is_private is_temp likes comments_count
		     tags
		     user { username profile { display_name } }
		   }
		 }`,
		map[string]any{"input": input}, in.TempOnly)
	if err != nil {
		return nil, nil, err
	}
	var wrap struct {
		Posts json.RawMessage `json:"posts"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		return nil, nil, err
	}
	res, err := rawJSONText(wrap.Posts)
	return res, nil, err
}

func getPostHandler(ctx context.Context, req *mcp.CallToolRequest, in getPostInput) (*mcp.CallToolResult, any, error) {
	if in.ID == "" && (in.Username == "" || in.URLSlug == "") {
		return nil, nil, errors.New("Provide `id`, or both `username` and `url_slug`.")
	}
	input := map[string]any{}
	if in.ID != "" {
		input["id"] = in.ID
	}
	if in.Username != "" {
		input["username"] = in.Username
	}
	if in.URLSlug != "" {
		input["url_slug"] = in.URLSlug
	}
	data, err := velogRequest(ctx,
		`query Post($input: ReadPostInput!) {
		   post(input: $input) {
		     id title body url_slug short_description thumbnail
		     released_at updated_at is_private is_temp is_markdown
		     likes views comments_count tags
		     user { username profile { display_name } }
		     series { id name }
		   }
		 }`,
		map[string]any{"input": input}, false)
	if err != nil {
		return nil, nil, err
	}
	var wrap struct {
		Post json.RawMessage `json:"post"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		return nil, nil, err
	}
	res, err := rawJSONText(wrap.Post)
	return res, nil, err
}

type writePostResult struct {
	ID        string `json:"id"`
	URLSlug   string `json:"url_slug"`
	IsTemp    bool   `json:"is_temp"`
	IsPrivate bool   `json:"is_private"`
	User      struct {
		Username string `json:"username"`
	} `json:"user"`
}

func writePostHandler(ctx context.Context, req *mcp.CallToolRequest, in writePostInput) (*mcp.CallToolResult, any, error) {
	if in.Title == "" {
		return nil, nil, errors.New("title is required")
	}
	payload := buildWritePayload(in, "")
	data, err := velogRequest(ctx,
		`mutation WritePost($input: WritePostInput!) {
		   writePost(input: $input) { id url_slug is_temp is_private user { username } }
		 }`,
		map[string]any{"input": payload}, true)
	if err != nil {
		return nil, nil, err
	}
	var wrap struct {
		WritePost writePostResult `json:"writePost"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		return nil, nil, err
	}
	out := map[string]any{
		"id":         wrap.WritePost.ID,
		"url_slug":   wrap.WritePost.URLSlug,
		"is_temp":    wrap.WritePost.IsTemp,
		"is_private": wrap.WritePost.IsPrivate,
		"user":       map[string]string{"username": wrap.WritePost.User.Username},
		"url":        fmt.Sprintf("https://velog.io/@%s/%s", wrap.WritePost.User.Username, wrap.WritePost.URLSlug),
	}
	res, err := jsonText(out)
	return res, nil, err
}

func editPostHandler(ctx context.Context, req *mcp.CallToolRequest, in editPostInput) (*mcp.CallToolResult, any, error) {
	if in.ID == "" {
		return nil, nil, errors.New("id is required")
	}
	if in.Title == "" {
		return nil, nil, errors.New("title is required")
	}
	payload := buildWritePayload(in.writePostInput, in.ID)
	data, err := velogRequest(ctx,
		`mutation EditPost($input: EditPostInput!) {
		   editPost(input: $input) { id url_slug is_temp is_private user { username } }
		 }`,
		map[string]any{"input": payload}, true)
	if err != nil {
		return nil, nil, err
	}
	var wrap struct {
		EditPost writePostResult `json:"editPost"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		return nil, nil, err
	}
	out := map[string]any{
		"id":         wrap.EditPost.ID,
		"url_slug":   wrap.EditPost.URLSlug,
		"is_temp":    wrap.EditPost.IsTemp,
		"is_private": wrap.EditPost.IsPrivate,
		"user":       map[string]string{"username": wrap.EditPost.User.Username},
		"url":        fmt.Sprintf("https://velog.io/@%s/%s", wrap.EditPost.User.Username, wrap.EditPost.URLSlug),
	}
	res, err := jsonText(out)
	return res, nil, err
}

func main() {
	loadTokenFile()

	server := mcp.NewServer(&mcp.Implementation{Name: "velog-mcp", Version: "0.1.0"}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "velog_whoami",
		Description: "Show the authenticated Velog user (requires VELOG_ACCESS_TOKEN). Use this to verify the token is valid.",
	}, whoamiHandler)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "velog_list_posts",
		Description: "List posts. Filter by username and/or tag. Use `cursor` (last seen post id) for pagination.",
	}, listPostsHandler)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "velog_get_post",
		Description: "Fetch a single post. Provide either `id`, or both `username` and `url_slug`.",
	}, getPostHandler)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "velog_write_post",
		Description: "Publish (or save as draft) a new Velog post. Requires VELOG_ACCESS_TOKEN.",
	}, writePostHandler)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "velog_edit_post",
		Description: "Edit an existing Velog post. Requires VELOG_ACCESS_TOKEN. All required fields must be re-sent (use velog_get_post first if needed).",
	}, editPostHandler)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		// Client disconnect / stdin EOF is the normal shutdown path.
		if errors.Is(err, io.EOF) {
			return
		}
		msg := err.Error()
		if msg == "server is closing: EOF" || msg == "server is closing" {
			return
		}
		log.Fatalf("[velog-mcp] fatal: %v", err)
	}
}
