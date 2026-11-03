package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/taghad/go-challenge/internal/estimator"
)

// addEvents handles POST /v1/events. Unknown JSON fields are ignored, so USS can add
// fields without breaking ES. Validation belongs to the service.
//
// The whole body is read before it is decoded. Its size is already bounded, an
// oversized body is always 413 whatever it starts with, and json.Unmarshal rejects
// data after the first value, which a json.Decoder would silently ignore.
func (a *api) addEvents(c *gin.Context) {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, a.config.MaxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(c, errBodyTooLarge)
			return
		}
		writeError(c, errMalformedBody)
		return
	}
	var request struct {
		Events []estimator.Event `json:"events"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		writeError(c, errMalformedBody)
		return
	}
	if err := a.service.Add(c.Request.Context(), request.Events); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
