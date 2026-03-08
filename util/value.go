package util

import (
	"errors"
	"net/http"

	"voidrun/model"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

var missingOrgError = errors.New("missing org context")

func GetOrgIDFromContext(c *gin.Context) (primitive.ObjectID, error) {
	orgIDHex := c.GetString("orgID")
	if orgIDHex == "" {
		c.JSON(http.StatusUnauthorized, model.NewErrorResponse("missing org context", ""))
		return primitive.NilObjectID, missingOrgError
	}
	return ParseObjectID(orgIDHex)
}

func GetUserIDFromContext(c *gin.Context) (primitive.ObjectID, error) {
	userIDHex := c.GetString("userID")
	if userIDHex == "" {
		c.JSON(http.StatusUnauthorized, model.NewErrorResponse("missing user context", ""))
		return primitive.NilObjectID, errors.New("missing user context")
	}
	return ParseObjectID(userIDHex)
}
