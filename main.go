package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

//go:embed index.html
var indexHTML string

const (
	defaultListenAddr  = ":8890"
	defaultBaseURL     = "https://hdhive.com"
	defaultTMDBBaseURL = "https://api.tmdb.org"
	defaultTimeout     = 20 * time.Second
	openAPIPath        = "/api/open"
)

type config struct {
	ListenAddr     string
	BaseURL        string
	DefaultAPIKey  string
	TMDBBaseURL    string
	DefaultTMDBKey string
	Timeout        time.Duration
}

type upstreamClient struct {
	baseURL        string
	defaultAPIKey  string
	tmdbBaseURL    string
	defaultTMDBKey string
	httpClient     *http.Client
}

type unlockRequest struct {
	Slug        string `json:"slug"`
	AllowPoints *bool  `json:"allow_points,omitempty"`
}

type shareDetailResponse struct {
	Success bool            `json:"success"`
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    shareDetailData `json:"data"`
}

type shareDetailData struct {
	Slug               string `json:"slug"`
	ActualUnlockPoints int    `json:"actual_unlock_points"`
	IsUnlocked         bool   `json:"is_unlocked"`
	IsFreeForUser      bool   `json:"is_free_for_user"`
	UnlockMessage      string `json:"unlock_message"`
}

type localError struct {
	Success     bool        `json:"success"`
	Code        string      `json:"code"`
	Message     string      `json:"message"`
	Description string      `json:"description,omitempty"`
	Data        interface{} `json:"data,omitempty"`
}

func main() {
	cfg := loadConfig()
	client := newUpstreamClient(cfg)

	router := gin.Default()
	registerRoutes(router, client)

	log.Printf("HDHive test client listening on %s (upstream: %s%s)", cfg.ListenAddr, cfg.BaseURL, openAPIPath)
	if err := router.Run(cfg.ListenAddr); err != nil {
		log.Fatalf("failed to start HDHive test client: %v", err)
	}
}

func loadConfig() config {
	timeout := defaultTimeout
	if raw := strings.TrimSpace(os.Getenv("HDHIVE_TIMEOUT_SECONDS")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			timeout = time.Duration(seconds) * time.Second
		}
	}

	baseURL := strings.TrimSpace(os.Getenv("HDHIVE_BASE_URL"))
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimSuffix(baseURL, "/")
	baseURL = strings.TrimSuffix(baseURL, openAPIPath)

	tmdbBaseURL := strings.TrimSpace(os.Getenv("TMDB_BASE_URL"))
	if tmdbBaseURL == "" {
		tmdbBaseURL = defaultTMDBBaseURL
	}
	tmdbBaseURL = normalizeTMDBBaseURL(tmdbBaseURL)

	listenAddr := strings.TrimSpace(os.Getenv("HDHIVE_LISTEN_ADDR"))
	if listenAddr == "" {
		listenAddr = defaultListenAddr
	}

	return config{
		ListenAddr:     listenAddr,
		BaseURL:        baseURL,
		DefaultAPIKey:  strings.TrimSpace(os.Getenv("HDHIVE_API_KEY")),
		TMDBBaseURL:    tmdbBaseURL,
		DefaultTMDBKey: strings.TrimSpace(os.Getenv("TMDB_API_KEY")),
		Timeout:        timeout,
	}
}

func newUpstreamClient(cfg config) *upstreamClient {
	return &upstreamClient{
		baseURL:        cfg.BaseURL,
		defaultAPIKey:  cfg.DefaultAPIKey,
		tmdbBaseURL:    cfg.TMDBBaseURL,
		defaultTMDBKey: cfg.DefaultTMDBKey,
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
		},
	}
}

func registerRoutes(router *gin.Engine, client *upstreamClient) {
	router.Use(corsMiddleware())

	router.GET("/", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(indexHTML))
	})

	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"success":  true,
			"message":  "ok",
			"upstream": client.baseURL + openAPIPath,
		})
	})

	router.GET(openAPIPath+"/ping", client.handleProxy(http.MethodGet, "/ping"))
	router.GET(openAPIPath+"/quota", client.handleProxy(http.MethodGet, "/quota"))
	router.GET(openAPIPath+"/usage", client.handleProxy(http.MethodGet, "/usage"))
	router.GET(openAPIPath+"/usage/today", client.handleProxy(http.MethodGet, "/usage/today"))
	router.GET(openAPIPath+"/resources/:type/:tmdb_id", client.handleProxy(http.MethodGet, "/resources/:type/:tmdb_id"))
	router.POST(openAPIPath+"/resources/unlock", client.handleUnlock())
	router.POST(openAPIPath+"/check/resource", client.handleProxy(http.MethodPost, "/check/resource"))

	router.GET(openAPIPath+"/shares", client.handleProxy(http.MethodGet, "/shares"))
	router.POST(openAPIPath+"/shares", client.handleProxy(http.MethodPost, "/shares"))
	router.GET(openAPIPath+"/shares/:slug", client.handleProxy(http.MethodGet, "/shares/:slug"))
	router.PATCH(openAPIPath+"/shares/:slug", client.handleProxy(http.MethodPatch, "/shares/:slug"))
	router.DELETE(openAPIPath+"/shares/:slug", client.handleProxy(http.MethodDelete, "/shares/:slug"))

	router.GET("/api/tmdb/configuration", client.handleTMDBProxy(http.MethodGet, "/configuration"))
	router.GET("/api/tmdb/configuration/primary_translations", client.handleTMDBProxy(http.MethodGet, "/configuration/primary_translations"))
	router.GET("/api/tmdb/search/:media_type", client.handleTMDBProxy(http.MethodGet, "/search/:media_type"))
	router.GET("/api/tmdb/:media_type/:media_id", client.handleTMDBProxy(http.MethodGet, "/:media_type/:media_id"))
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		headers := c.Writer.Header()
		headers.Set("Access-Control-Allow-Origin", "*")
		headers.Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, X-TMDB-API-Key")
		headers.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		headers.Set("Access-Control-Expose-Headers", "Content-Type, X-RateLimit-Reset, X-Endpoint-Limit, X-Endpoint-Remaining, Retry-After")

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

func (u *upstreamClient) handleProxy(method string, pathTemplate string) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, status, headers, err := u.forwardRequest(
			c.Request.Context(),
			method,
			expandPath(pathTemplate, c),
			copyQuery(c.Request.URL.Query()),
			readRequestBody(c.Request),
			u.resolveAPIKey(c.GetHeader("X-API-Key")),
			c.GetHeader("Content-Type"),
		)
		if err != nil {
			respondLocalError(c, http.StatusBadGateway, "UPSTREAM_REQUEST_FAILED", err.Error(), "")
			return
		}

		copyResponseHeaders(c.Writer.Header(), headers)
		c.Data(status, contentTypeFromHeaders(headers), body)
	}
}

func (u *upstreamClient) handleUnlock() gin.HandlerFunc {
	return func(c *gin.Context) {
		rawBody := readRequestBody(c.Request)
		if len(rawBody) == 0 {
			respondLocalError(c, http.StatusBadRequest, "400", "request body is required", "请求体不能为空")
			return
		}

		var req unlockRequest
		if err := json.Unmarshal(rawBody, &req); err != nil {
			respondLocalError(c, http.StatusBadRequest, "400", "invalid json body", "请求体 JSON 格式无效")
			return
		}

		req.Slug = strings.TrimSpace(req.Slug)
		if req.Slug == "" {
			respondLocalError(c, http.StatusBadRequest, "400", "slug is required", "缺少 slug")
			return
		}

		apiKey := u.resolveAPIKey(c.GetHeader("X-API-Key"))
		if apiKey == "" {
			respondLocalError(c, http.StatusUnauthorized, "MISSING_API_KEY", "API Key is required", "请通过请求头或环境变量提供 X-API-Key")
			return
		}

		allowPoints := false
		if req.AllowPoints != nil {
			allowPoints = *req.AllowPoints
		}

		if !allowPoints {
			detail, errResp := u.lookupShareDetailForUnlock(c.Request.Context(), apiKey, req.Slug)
			if errResp != nil {
				copyResponseHeaders(c.Writer.Header(), errResp.Headers)
				c.Data(errResp.StatusCode, contentTypeFromHeaders(errResp.Headers), errResp.Body)
				return
			}

			if !isSafeToUnlock(detail) {
				respondLocalError(
					c,
					http.StatusConflict,
					"POINTS_REQUIRED",
					"resource requires points; set allow_points=true to unlock",
					"该资源当前需要扣积分。默认不允许扣积分解锁，请显式传 allow_points=true 后再执行。",
					gin.H{
						"slug":                 detail.Slug,
						"actual_unlock_points": detail.ActualUnlockPoints,
						"is_unlocked":          detail.IsUnlocked,
						"is_free_for_user":     detail.IsFreeForUser,
						"unlock_message":       detail.UnlockMessage,
					},
				)
				return
			}
		}

		upstreamBody, _ := json.Marshal(gin.H{"slug": req.Slug})
		body, status, headers, err := u.forwardRequest(
			c.Request.Context(),
			http.MethodPost,
			"/resources/unlock",
			nil,
			upstreamBody,
			apiKey,
			"application/json",
		)
		if err != nil {
			respondLocalError(c, http.StatusBadGateway, "UPSTREAM_REQUEST_FAILED", err.Error(), "")
			return
		}

		copyResponseHeaders(c.Writer.Header(), headers)
		c.Data(status, contentTypeFromHeaders(headers), body)
	}
}

func (u *upstreamClient) handleTMDBProxy(method string, pathTemplate string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if mediaType := strings.TrimSpace(c.Param("media_type")); mediaType != "" {
			switch mediaType {
			case "movie", "tv", "multi":
			default:
				respondLocalError(c, http.StatusBadRequest, "400", "invalid media_type", "media_type 只能是 movie、tv 或 multi")
				return
			}
		}

		tmdbKey := u.resolveTMDBAPIKey(c.GetHeader("X-TMDB-API-Key"))
		if tmdbKey == "" {
			respondLocalError(c, http.StatusUnauthorized, "MISSING_TMDB_API_KEY", "TMDB API Key is required", "请通过请求头 X-TMDB-API-Key 或环境变量 TMDB_API_KEY 提供 TMDB API Key")
			return
		}

		body, status, headers, err := u.forwardTMDBRequest(
			c.Request.Context(),
			method,
			expandPath(pathTemplate, c),
			copyQuery(c.Request.URL.Query()),
			readRequestBody(c.Request),
			tmdbKey,
			c.GetHeader("Content-Type"),
		)
		if err != nil {
			respondLocalError(c, http.StatusBadGateway, "TMDB_REQUEST_FAILED", err.Error(), "")
			return
		}

		copyResponseHeaders(c.Writer.Header(), headers)
		c.Data(status, contentTypeFromHeaders(headers), body)
	}
}

type proxyResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

func (u *upstreamClient) lookupShareDetailForUnlock(ctx context.Context, apiKey, slug string) (*shareDetailData, *proxyResponse) {
	normalizedSlug := normalizeSlug(slug)
	body, status, headers, err := u.forwardRequest(
		ctx,
		http.MethodGet,
		"/shares/"+url.PathEscape(normalizedSlug),
		nil,
		nil,
		apiKey,
		"",
	)
	if err != nil {
		return nil, &proxyResponse{
			StatusCode: http.StatusBadGateway,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body: mustMarshal(localError{
				Success: false,
				Code:    "UPSTREAM_REQUEST_FAILED",
				Message: err.Error(),
			}),
		}
	}

	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, &proxyResponse{StatusCode: status, Headers: headers, Body: body}
	}

	var parsed shareDetailResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, &proxyResponse{
			StatusCode: http.StatusBadGateway,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body: mustMarshal(localError{
				Success:     false,
				Code:        "UPSTREAM_RESPONSE_INVALID",
				Message:     "failed to parse share detail response",
				Description: "上游 shares detail 接口返回的数据结构无法解析",
			}),
		}
	}

	parsed.Data.Slug = normalizedSlug
	return &parsed.Data, nil
}

func (u *upstreamClient) forwardRequest(
	ctx context.Context,
	method string,
	path string,
	query url.Values,
	body []byte,
	apiKey string,
	contentType string,
) ([]byte, int, http.Header, error) {
	targetURL := u.baseURL + openAPIPath + path
	if len(query) > 0 {
		targetURL += "?" + query.Encode()
	}

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, targetURL, bodyReader)
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	if len(body) > 0 {
		if contentType == "" {
			contentType = "application/json"
		}
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, nil, err
	}

	return respBody, resp.StatusCode, resp.Header.Clone(), nil
}

func (u *upstreamClient) resolveAPIKey(requestKey string) string {
	if strings.TrimSpace(requestKey) != "" {
		return strings.TrimSpace(requestKey)
	}
	return u.defaultAPIKey
}

func (u *upstreamClient) resolveTMDBAPIKey(requestKey string) string {
	if strings.TrimSpace(requestKey) != "" {
		return strings.TrimSpace(requestKey)
	}
	return u.defaultTMDBKey
}

func (u *upstreamClient) forwardTMDBRequest(
	ctx context.Context,
	method string,
	path string,
	query url.Values,
	body []byte,
	apiKey string,
	contentType string,
) ([]byte, int, http.Header, error) {
	targetQuery := copyQuery(query)
	if targetQuery == nil {
		targetQuery = make(url.Values)
	}
	targetQuery.Set("api_key", apiKey)

	targetURL := u.tmdbBaseURL + path
	if len(targetQuery) > 0 {
		targetURL += "?" + targetQuery.Encode()
	}

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, targetURL, bodyReader)
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		if contentType == "" {
			contentType = "application/json"
		}
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, nil, err
	}

	return respBody, resp.StatusCode, resp.Header.Clone(), nil
}

func expandPath(template string, c *gin.Context) string {
	result := template
	for _, key := range []string{"type", "tmdb_id", "slug", "media_type", "media_id"} {
		result = strings.ReplaceAll(result, ":"+key, url.PathEscape(c.Param(key)))
	}
	return result
}

func copyQuery(values url.Values) url.Values {
	if values == nil {
		return nil
	}
	cloned := make(url.Values, len(values))
	for key, items := range values {
		cloned[key] = append([]string(nil), items...)
	}
	return cloned
}

func readRequestBody(req *http.Request) []byte {
	if req == nil || req.Body == nil {
		return nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	return body
}

func normalizeSlug(slug string) string {
	replacer := strings.NewReplacer("-", "", " ", "")
	return strings.ToLower(strings.TrimSpace(replacer.Replace(slug)))
}

func isSafeToUnlock(detail *shareDetailData) bool {
	if detail == nil {
		return false
	}
	if detail.IsUnlocked || detail.IsFreeForUser {
		return true
	}
	return detail.ActualUnlockPoints <= 0
}

func normalizeTMDBBaseURL(value string) string {
	normalized := strings.TrimSpace(value)
	if normalized == "" {
		normalized = defaultTMDBBaseURL
	}
	normalized = strings.TrimSuffix(normalized, "/")
	if strings.HasSuffix(normalized, "/3") {
		return normalized
	}
	return normalized + "/3"
}

func copyResponseHeaders(dst, src http.Header) {
	if dst == nil || src == nil {
		return
	}
	for _, key := range []string{
		"Content-Type",
		"X-RateLimit-Reset",
		"X-Endpoint-Limit",
		"X-Endpoint-Remaining",
		"Retry-After",
	} {
		values := src.Values(key)
		if len(values) == 0 {
			continue
		}
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func contentTypeFromHeaders(headers http.Header) string {
	if headers == nil {
		return "application/json"
	}
	if value := headers.Get("Content-Type"); value != "" {
		return value
	}
	return "application/json"
}

func respondLocalError(c *gin.Context, status int, code, message, description string, data ...interface{}) {
	resp := localError{
		Success:     false,
		Code:        code,
		Message:     message,
		Description: description,
	}
	if len(data) > 0 {
		resp.Data = data[0]
	}
	c.JSON(status, resp)
}

func mustMarshal(v interface{}) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

func init() {
	gin.SetMode(resolveGinMode())
}

func resolveGinMode() string {
	mode := strings.TrimSpace(os.Getenv("GIN_MODE"))
	switch mode {
	case gin.DebugMode, gin.ReleaseMode, gin.TestMode:
		return mode
	case "":
		return gin.ReleaseMode
	default:
		return gin.ReleaseMode
	}
}
