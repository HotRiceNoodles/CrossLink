package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/crosslink/internal/apidoc"
)

// OpenAPIHandler serves the bundled OpenAPI spec (JSON) at /openapi.json so
// tools (Postman/Insomnia, codegen) can import it by URL. Public, no auth —
// the spec is published API documentation.
func OpenAPIHandler(c *gin.Context) {
	c.Data(http.StatusOK, "application/json; charset=utf-8", apidoc.SpecJSON)
}
