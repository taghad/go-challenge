// Package httpapi exposes the estimator over HTTP.
package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/taghad/go-challenge/internal/estimator"
	"github.com/taghad/go-challenge/internal/web"
)

// Config holds the limits of the HTTP layer.
type Config struct {
	MaxBodyBytes      int64 // largest body POST /v1/events accepts
	MaxInflightIngest int   // concurrent POST /v1/events requests
}

// api holds what the handlers need. It depends on the concrete service, because the
// service already has a fake at its own boundary, the Store.
type api struct {
	service *estimator.Service
	config  Config
}

// index handles GET /, which serves the frontend.
func index(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", web.Index)
}

// NewRouter returns the HTTP handler of ES. It has Recovery, so a panic in one request
// returns 500 instead of ending the process. It has no request Logger, because a line
// per request is a cost on the ingestion path.
func NewRouter(service *estimator.Service, config Config) *gin.Engine {
	a := &api{service: service, config: config}
	router := gin.New()
	router.Use(gin.Recovery())
	router.POST("/v1/events", ingestLimit(config.MaxInflightIngest), a.addEvents)
	router.GET("/v1/segments/:segment/estimate", a.estimate)
	router.GET("/healthz", healthz)
	router.GET("/readyz", a.readyz)
	router.GET("/", index)
	return router
}
