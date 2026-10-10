// Package middleware 存放 gin HTTP 中间件（仅 API 层允许依赖 gin）。
package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/GenJi77JYXC/llm-gateway/internal/model"
)

// ClientAuth 校验 Authorization: Bearer <key>。
// keys 为允许的客户端 key 列表（Week 1 静态配置，后续替换为 DB 持久化）。
func ClientAuth(keys []string) gin.HandlerFunc {
	allowed := make([][]byte, 0, len(keys))
	for _, k := range keys {
		allowed = append(allowed, []byte(k))
	}
	return func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		prefix := "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, model.ErrorBody{
				Error: model.APIError{
					Message: "missing or malformed Authorization header, expected: Bearer <key>",
					Type:    "invalid_request_error",
					Code:    "invalid_api_key",
				},
			})
			return
		}
		presented := []byte(strings.TrimPrefix(auth, prefix))
		for _, key := range allowed {
			// 常量时间比较，避免时序侧信道
			if subtle.ConstantTimeCompare(presented, key) == 1 {
				c.Next()
				return
			}
		}
		c.AbortWithStatusJSON(http.StatusUnauthorized, model.ErrorBody{
			Error: model.APIError{
				Message: "invalid api key",
				Type:    "invalid_request_error",
				Code:    "invalid_api_key",
			},
		})
	}
}
