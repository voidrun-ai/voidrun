package handler

import "voidrun/internal/service"

type UserHandler struct {
	userService *service.UserService
}

// NewUserHandler creates a new User handler
func NewUserHandler(userService *service.UserService) *UserHandler {
	return &UserHandler{userService: userService}
}
