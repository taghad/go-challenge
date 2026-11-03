package httpapi

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

type estimateResponse struct {
	Segment  string `json:"segment"`
	Estimate uint64 `json:"estimate"`
	AsOf     string `json:"as_of"`
}

// estimate handles GET /v1/segments/:segment/estimate. as_of is in UTC and in whole
// seconds, because RFC3339 without a fraction drops anything smaller.
func (a *api) estimate(c *gin.Context) {
	segment := c.Param("segment")
	estimate, err := a.service.Estimate(c.Request.Context(), segment)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, estimateResponse{
		Segment:  segment,
		Estimate: estimate.Value,
		AsOf:     estimate.AsOf.UTC().Format(time.RFC3339),
	})
}
