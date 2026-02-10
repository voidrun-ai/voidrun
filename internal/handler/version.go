package handler

import (
	"net/http"

	"voidrun/internal/version"

	"github.com/gin-gonic/gin"
)

type VersionHandler struct {}

func NewVersionHandler() *VersionHandler {
	return &VersionHandler{}
}

func (h *VersionHandler) Get(c *gin.Context) {
	c.JSON(http.StatusOK, version.Get())
}
