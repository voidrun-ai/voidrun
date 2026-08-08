package service

import (
	"context"
	"testing"

	"voidrun/config"
	"voidrun/model"
	"voidrun/repository"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type captureCtxImageRepo struct {
	gotCtx context.Context
	img    *model.Image
}

func (r *captureCtxImageRepo) Create(context.Context, *model.Image) (*model.Image, error) {
	panic("unexpected")
}
func (r *captureCtxImageRepo) FindByIDAndOrgOrSystem(context.Context, primitive.ObjectID, primitive.ObjectID) (*model.Image, error) {
	panic("unexpected")
}
func (r *captureCtxImageRepo) Find(context.Context, primitive.ObjectID, interface{}, options.FindOptions) ([]*model.Image, error) {
	panic("unexpected")
}
func (r *captureCtxImageRepo) DeleteByIDAndOrg(context.Context, primitive.ObjectID, primitive.ObjectID) (bool, error) {
	panic("unexpected")
}
func (r *captureCtxImageRepo) Count(context.Context, primitive.ObjectID, interface{}) (int64, error) {
	panic("unexpected")
}
func (r *captureCtxImageRepo) Exists(context.Context, primitive.ObjectID, primitive.ObjectID) bool {
	panic("unexpected")
}
func (r *captureCtxImageRepo) GetLatestByNameForOrg(ctx context.Context, _ string, _ primitive.ObjectID) (*model.Image, error) {
	r.gotCtx = ctx
	return r.img, nil
}
func (r *captureCtxImageRepo) ResolveImage(context.Context, primitive.ObjectID, string) (*model.Image, error) {
	panic("unexpected")
}
func (r *captureCtxImageRepo) EnsureSystemImage(model.Image) error { panic("unexpected") }
func (r *captureCtxImageRepo) DeactivateStaleSystemImages(context.Context, []model.Image) error {
	panic("unexpected")
}

var _ repository.IImageRepository = (*captureCtxImageRepo)(nil)

func TestGetLatestByNameForOrgPassesContext(t *testing.T) {
	t.Parallel()

	type ctxKey struct{}
	want := context.WithValue(context.Background(), ctxKey{}, "from-caller")

	repo := &captureCtxImageRepo{
		img: &model.Image{Name: "code", Tag: "1"},
	}
	svc := NewImageService(&config.Config{}, repo)

	got, err := svc.GetLatestByNameForOrg(want, "code", primitive.NewObjectID())
	if err != nil {
		t.Fatalf("GetLatestByNameForOrg: %v", err)
	}
	if got == nil || got.Name != "code" {
		t.Fatalf("unexpected image: %#v", got)
	}
	if repo.gotCtx != want {
		t.Fatalf("repo did not receive caller context")
	}
}
