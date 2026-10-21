package service

import (
	"context"
	"errors"
	"testing"

	"voidrun/model"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestDeleteRefusesCreatingSandbox(t *testing.T) {
	t.Parallel()
	s := &SandboxService{}
	sb := &model.Sandbox{ID: primitive.NewObjectID(), Status: "creating"}
	err := s.deleteLockedSandbox(context.Background(), primitive.NewObjectID(), sb.ID.Hex(), sb)
	if !errors.Is(err, ErrSandboxCreating) {
		t.Fatalf("err = %v, want ErrSandboxCreating", err)
	}
}
