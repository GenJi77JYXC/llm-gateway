// Package middleware 存放 gin HTTP 中间件（仅 API 层允许依赖 gin）。
package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/GenJi77JYXC/llm-gateway/internal/config"
	"github.com/GenJi77JYXC/llm-gateway/internal/model"
)

// CtxKeyID 是鉴权通过后注入 gin context 的客户端标识 key，
// 值为 auth.client_keys.name，handler 层用 GetString(CtxKeyID) 读取。
const CtxKeyID = "key_id"

// ClientAuth 校验 Authorization: Bearer <key>，通过后把客户端标识（name）
// 注入 context 供计量使用。keys 来自静态配置（Week 1 引入，后续替换为 DB 持久化）。
func ClientAuth(keys []config.ClientKey) gin.HandlerFunc {
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
		for _, ck := range keys {
			// 常量时间比较，避免时序侧信道
			if subtle.ConstantTimeCompare(presented, []byte(ck.Key)) == 1 {
				c.Set(CtxKeyID, ck.Name)
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
