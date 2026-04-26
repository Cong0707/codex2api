package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRegisterRoutes_OpenAICompatibilityPaths(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	h := &Handler{
		// 通过静态 key 触发鉴权路径，避免依赖数据库实例。
		configKeys: map[string]bool{"sk-test": true},
	}
	h.RegisterRoutes(r)

	cases := []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/v1/chat/completions"},
		{method: http.MethodPost, path: "/chat/completions"},
		{method: http.MethodPost, path: "/v1/responses"},
		{method: http.MethodPost, path: "/responses"},
		{method: http.MethodPost, path: "/v1/responses/compact"},
		{method: http.MethodPost, path: "/responses/compact"},
		{method: http.MethodPost, path: "/v1/messages"},
		{method: http.MethodPost, path: "/messages"},
		{method: http.MethodPost, path: "/backend-api/codex/responses"},
		{method: http.MethodPost, path: "/backend-api/codex/responses/compact"},
		{method: http.MethodGet, path: "/v1/models"},
		{method: http.MethodGet, path: "/models"},
	}

	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status=%d, want %d", tc.method, tc.path, rec.Code, http.StatusUnauthorized)
		}
	}
}
