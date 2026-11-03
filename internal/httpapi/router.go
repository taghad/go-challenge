// Package httpapi exposes the estimator over HTTP.
package httpapi

import (
	"github.com/gin-gonic/gin"

	"github.com/taghad/go-challenge/internal/estimator"
)

// Config holds the limits of the HTTP layer.
type Config struct {
	MaxBodyBytes int64 // largest body POST /v1/events accepts
}

// api holds what the handlers need. It depends on the concrete service, because the
// service already has a fake at its own boundary, the Store.
type api struct {
	service *estimator.Service
	config  Config
}

// NewRouter returns the HTTP handler of ES.
func NewRouter(service *estimator.Service, config Config) *gin.Engine {
	a := &api{service: service, config: config}
	router := gin.New()
	router.POST("/v1/events", a.addEvents)
	router.GET("/v1/segments/:segment/estimate", a.estimate)
	return router
}
