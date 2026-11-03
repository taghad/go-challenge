package httpapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/taghad/go-challenge/internal/estimator"
)

// retryAfterSeconds is the fixed value of Retry-After on a 503, 1 second [E].
const retryAfterSeconds = "1"

// Errors of the HTTP layer itself.
var (
	errMalformedBody = errors.New("request body is not valid JSON")
	errBodyTooLarge  = errors.New("request body is too large")
)

// errorResponse is the body of every error. Index and Field are set only for an
// invalid event. Index is a pointer so that the first event, index 0, is not omitted.
type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Index   *int   `json:"index,omitempty"`
	Field   string `json:"field,omitempty"`
}

// writeError turns an error into a response.
//
// A cancelled request gets no response, because its client has gone. This looks at the
// request's context and not at the error, because a store timeout also wraps a context
// error, and that one must still return 503 to a client that is waiting.
//
// Anything not recognized here came from the store. Its text is logged and not
// returned, because it describes the internals of ES.
func writeError(c *gin.Context, err error) {
	if c.Request.Context().Err() != nil {
		return
	}
	var invalid *estimator.ValidationError
	switch {
	case errors.Is(err, errBodyTooLarge):
		writeDetail(c, http.StatusRequestEntityTooLarge, errorDetail{Code: "body_too_large", Message: err.Error()})
	case errors.Is(err, estimator.ErrBatchTooLarge):
		writeDetail(c, http.StatusRequestEntityTooLarge, errorDetail{Code: "batch_too_large", Message: err.Error()})
	case errors.Is(err, errMalformedBody):
		writeDetail(c, http.StatusBadRequest, errorDetail{Code: "malformed_json", Message: err.Error()})
	case errors.Is(err, estimator.ErrEmptyBatch):
		writeDetail(c, http.StatusBadRequest, errorDetail{Code: "empty_batch", Message: err.Error()})
	case errors.As(err, &invalid):
		writeDetail(c, http.StatusBadRequest, errorDetail{
			Code:    "invalid_event",
			Message: err.Error(),
			Index:   &invalid.Index,
			Field:   invalid.Field,
		})
	case errors.Is(err, estimator.ErrInvalidSegment):
		writeDetail(c, http.StatusBadRequest, errorDetail{Code: "invalid_segment", Message: err.Error()})
	case errors.Is(err, estimator.ErrOverloaded):
		writeUnavailable(c, "overloaded", err.Error())
	default:
		slog.Error("store request failed", "error", err)
		writeUnavailable(c, "store_unavailable", "the store is unavailable")
	}
}

func writeUnavailable(c *gin.Context, code, message string) {
	c.Header("Retry-After", retryAfterSeconds)
	writeDetail(c, http.StatusServiceUnavailable, errorDetail{Code: code, Message: message})
}

func writeDetail(c *gin.Context, status int, detail errorDetail) {
	c.JSON(status, errorResponse{Error: detail})
}
