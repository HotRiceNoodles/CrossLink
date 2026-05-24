package middleware

import (
	"bytes"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

const BodyKey = "_body_bytes"

func ReadBody(maxSize int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		bodyBytes, err := io.ReadAll(io.LimitReader(c.Request.Body, maxSize))
		c.Request.Body.Close()
		if err != nil {
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
		c.Set(BodyKey, bodyBytes)
		c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		c.Next()
	}
}

func GetBodyBytes(c *gin.Context) []byte {
	val, _ := c.Get(BodyKey)
	if b, ok := val.([]byte); ok {
		return b
	}
	return nil
}
