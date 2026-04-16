package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"letshare-server/internal/config"

	"github.com/gin-gonic/gin"
)

type GLMHandler struct {
	apiKey      string
	baseURL     string
	modelOpus   string
	modelSonnet string
	modelVision string
	httpClient  *http.Client
}

func NewGLMHandler(cfg config.GLM) *GLMHandler {
	return &GLMHandler{
		apiKey:      cfg.APIKey,
		baseURL:     strings.TrimRight(cfg.BaseURL, "/"),
		modelOpus:   cfg.ModelOpus,
		modelSonnet: cfg.ModelSonnet,
		modelVision: cfg.ModelVision,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

func (h *GLMHandler) Proxy(c *gin.Context) {
	if h.apiKey == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "GLM_API_KEY 未配置"})
		return
	}

	upstreamPath := strings.TrimPrefix(c.Param("path"), "/")
	if upstreamPath == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少上游路径"})
		return
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "读取请求体失败"})
		return
	}

	body, err = h.rewriteModelAlias(body, c.GetHeader("Content-Type"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	upstreamURL := h.baseURL + "/" + upstreamPath
	if rawQuery := c.Request.URL.RawQuery; rawQuery != "" {
		upstreamURL += "?" + rawQuery
	}

	upstreamReq, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "上游请求创建失败"})
		return
	}

	h.copyRequestHeaders(c, upstreamReq)
	upstreamReq.Header.Set("Authorization", "Bearer "+h.apiKey)

	resp, err := h.httpClient.Do(upstreamReq)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "调用 GLM 失败"})
		return
	}
	defer resp.Body.Close()

	h.copyResponseHeaders(c, resp)
	c.Status(resp.StatusCode)

	if err := streamResponse(c.Writer, resp.Body); err != nil {
		c.Abort()
	}
}

func (h *GLMHandler) rewriteModelAlias(body []byte, contentType string) ([]byte, error) {
	if len(body) == 0 || !strings.Contains(strings.ToLower(contentType), "application/json") {
		return body, nil
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}

	model, ok := payload["model"].(string)
	if !ok || strings.TrimSpace(model) == "" {
		return body, nil
	}

	modelName, err := h.resolveModel(model)
	if err != nil {
		return nil, err
	}
	payload["model"] = modelName

	return json.Marshal(payload)
}

func (h *GLMHandler) copyRequestHeaders(c *gin.Context, upstreamReq *http.Request) {
	for key, values := range c.Request.Header {
		lowerKey := strings.ToLower(key)
		if lowerKey == "host" || lowerKey == "authorization" || lowerKey == "content-length" {
			continue
		}
		for _, value := range values {
			upstreamReq.Header.Add(key, value)
		}
	}
}

func (h *GLMHandler) copyResponseHeaders(c *gin.Context, resp *http.Response) {
	for key, values := range resp.Header {
		lowerKey := strings.ToLower(key)
		if lowerKey == "content-length" || lowerKey == "transfer-encoding" || lowerKey == "content-encoding" || strings.HasPrefix(lowerKey, "access-control-") {
			continue
		}
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}
}

func streamResponse(w gin.ResponseWriter, body io.Reader) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			w.Flush()
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (h *GLMHandler) resolveModel(requested string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(requested)) {
	case "opus", "glm_model_opus":
		if h.modelOpus == "" {
			return "", errors.New("GLM_MODEL_OPUS 未配置")
		}
		return h.modelOpus, nil
	case "sonnet", "glm_model_sonnet":
		if h.modelSonnet == "" {
			return "", errors.New("GLM_MODEL_SONNET 未配置")
		}
		return h.modelSonnet, nil
	case "vision", "glm_model_vision":
		if h.modelVision == "" {
			return "", errors.New("GLM_MODEL_VISION 未配置")
		}
		return h.modelVision, nil
	default:
		return requested, nil
	}
}
