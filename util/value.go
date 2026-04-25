package util

import (
	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson/primitive"
)


func GetOrgIDFromContext(c *gin.Context) (primitive.ObjectID, error) {
	orgIDHex := c.GetString("orgID")
	if orgIDHex == "" {
		return primitive.NilObjectID, ErrUnauthorized("missing org context")
	}
	id, err := ParseObjectID(orgIDHex)
	if err != nil {
		return primitive.NilObjectID, ErrBadRequest("invalid org id format", err.Error())
	}
	return id, nil
}

func GetUserIDFromContext(c *gin.Context) (primitive.ObjectID, error) {
	userIDHex := c.GetString("userID")
	if userIDHex == "" {
		return primitive.NilObjectID, ErrUnauthorized("missing user context")
	}
	id, err := ParseObjectID(userIDHex)
	if err != nil {
		return primitive.NilObjectID, ErrBadRequest("invalid user id format", err.Error())
	}
	return id, nil
}

