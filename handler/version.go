package handler

import (
	"net/http"

	"voidrun/util"

	"github.com/gin-gonic/gin"
)

type VersionHandler struct{}

func NewVersionHandler() *VersionHandler {
	return &VersionHandler{}
}

func (h *VersionHandler) Get(c *gin.Context) {
	c.JSON(http.StatusOK, util.Get())
}
