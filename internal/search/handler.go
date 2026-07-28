package search

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/requestbody"
)

const maxRequestBodySize = 16 << 10

// Handler serves authenticated normalized web search requests.
type Handler struct {
	service *Service
}

// NewHandler creates a web search handler.
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// Web handles POST /search/web.
func (h *Handler) Web(c *gin.Context) {
	requestbody.Limit(c, maxRequestBodySize)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	var req Request
	if err := decoder.Decode(&req); err != nil {
		if requestbody.TooLarge(err) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body is too large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid search request"})
		return
	}
	if err := ensureEOF(decoder); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid search request"})
		return
	}
	response, err := h.service.Search(c.Request.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidRequest):
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid search request"})
		case errors.Is(err, ErrProviderUnavailable):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "search provider is unavailable"})
		default:
			c.JSON(http.StatusBadGateway, gin.H{"error": "search provider request failed"})
		}
		return
	}
	c.JSON(http.StatusOK, response)
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("request must contain one JSON value")
	}
	return nil
}
