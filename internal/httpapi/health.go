package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// healthz handles GET /healthz. It has no dependencies, so it says only that the
// process is alive, and a Redis outage does not make a restart look necessary.
func healthz(c *gin.Context) {
	c.Status(http.StatusOK)
}

// readyz handles GET /readyz, which reports whether Redis is reachable.
func (a *api) readyz(c *gin.Context) {
	if err := a.service.Ping(c.Request.Context()); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusOK)
}
