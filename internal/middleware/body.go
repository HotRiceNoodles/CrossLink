package middleware

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const BodyKey = "_body_bytes"

func ReadBody(maxSize int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Multipart file uploads must NOT be buffered here: io.ReadAll(LimitReader)
		// silently truncates at maxSize (10MB) and would corrupt the multipart
		// framing. Leave the body untouched so handlers can stream the file via
		// FormFile; the global MaxBytesReader ceiling still applies upstream.
		//
		// Scope this exemption to known file-upload endpoints only — exempting ALL
		// multipart lets a large JSON payload masquerade as multipart to bypass the
		// 10MB buffer (5x, up to the 50MB global ceiling) on arbitrary routes.
		if isFileUploadPath(c.Request.URL.Path) &&
			strings.HasPrefix(c.Request.Header.Get("Content-Type"), "multipart/form-data") {
			c.Next()
			return
		}
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

// isFileUploadPath reports whether the route accepts multipart file uploads and
// therefore must skip body buffering (which would truncate/corrupt large files).
// Currently: audio transcription/translation and image edits.
func isFileUploadPath(path string) bool {
	return strings.HasPrefix(path, "/v1/audio/") ||
		strings.HasPrefix(path, "/v1/images/")
}
