package httpapi

import "github.com/gin-gonic/gin"

// ingestLimit returns middleware that limits concurrent ingestion requests. It never
// waits: when full it rejects at once, so ES builds no internal queue, its memory stays
// bounded, and USS absorbs a spike through retries.
//
// It runs before the handler reads the body, so a rejected request costs no read and
// no memory for its body.
func ingestLimit(maxInflight int) gin.HandlerFunc {
	slots := make(chan struct{}, maxInflight)
	return func(c *gin.Context) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			c.Next()
		default:
			writeError(c, errIngestionFull)
			c.Abort()
		}
	}
}
