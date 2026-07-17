package requestbody

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Limit replaces the request body with a reader that returns MaxBytesError
// after limit bytes.
func Limit(c *gin.Context, limit int64) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
}

// TooLarge reports whether reading a limited request exceeded its limit.
func TooLarge(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.As(err, &maxBytesErr)
}
