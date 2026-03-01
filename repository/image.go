package repository

import (
	"context"
	"time"

	"voidrun/config"
	"voidrun/model"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type IImageRepository interface {
	Create(ctx context.Context, orgID primitive.ObjectID, image *model.Image) (*model.Image, error)
	FindByIDAndOrgOrSystem(ctx context.Context, id, orgID primitive.ObjectID) (*model.Image, error)
	Find(ctx context.Context, orgID primitive.ObjectID, filter interface{}, opts options.FindOptions) ([]*model.Image, error)
	DeleteByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID) (bool, error)
	Count(ctx context.Context, orgID primitive.ObjectID, filter interface{}) (int64, error)
	Exists(ctx context.Context, orgID, id primitive.ObjectID) bool

	GetLatestByNameForOrg(name string, orgID primitive.ObjectID) (*model.Image, error)
	EnsureSystemImage(img model.Image) error
}

// ImageRepository manages images in MongoDB
type ImageRepository struct {
	cfg        *config.Config
	collection *mongo.Collection
}

func NewImageRepository(cfg *config.Config, db *mongo.Database) IImageRepository {
	return &ImageRepository{
		cfg:        cfg,
		collection: db.Collection("images"),
	}
}

// Add creates a new image
func (r *ImageRepository) Create(ctx context.Context, orgID primitive.ObjectID, img *model.Image) (*model.Image, error) {
	img.OrgID = orgID
	img.CreatedAt = time.Now()
	if img.ID.IsZero() {
		img.ID = primitive.NewObjectID()
	}
	_, err := r.collection.InsertOne(ctx, img)
	if err != nil {
		return nil, err
	}
	return img, nil
}

func (r *ImageRepository) FindByIDAndOrgOrSystem(ctx context.Context, id, orgID primitive.ObjectID) (*model.Image, error) {
	var img *model.Image
	filter := bson.M{
		"_id": id,
		"$or": []bson.M{
			{"orgId": orgID},
			{"system": true},
		},
	}
	err := r.collection.FindOne(ctx, filter).Decode(&img)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return img, nil
}

// GetByNameTag retrieves an image by name and tag
func (r *ImageRepository) GetLatestByNameForOrg(name string, orgID primitive.ObjectID) (*model.Image, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var img *model.Image
	filter := bson.M{
		"name": name,
		"$or": []bson.M{
			{"orgId": orgID},
			{"system": true},
		},
	}
	err := r.collection.FindOne(ctx, filter, options.FindOne().SetSort(bson.D{{Key: "_id", Value: -1}})).Decode(&img)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return img, nil
}

// GetAll retrieves all images
func (r *ImageRepository) Find(ctx context.Context, orgID primitive.ObjectID, filter interface{}, opts options.FindOptions) ([]*model.Image, error) {
	baseFilter := bson.M{}
	if filterMap, ok := filter.(bson.M); ok {
		for k, v := range filterMap {
			baseFilter[k] = v
		}
	}
	visibilityFilter := bson.M{
		"$or": []bson.M{
			{"orgId": orgID},
			{"system": true},
		},
	}
	finalFilter := visibilityFilter
	if len(baseFilter) > 0 {
		finalFilter = bson.M{
			"$and": []bson.M{baseFilter, visibilityFilter},
		}
	}

	cursor, err := r.collection.Find(ctx, finalFilter, &opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var images []*model.Image
	if err = cursor.All(ctx, &images); err != nil {
		return nil, err
	}
	return images, nil
}

// Delete removes an image
func (r *ImageRepository) DeleteByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID) (bool, error) {
	// System images cannot be deleted from org-scoped endpoints.
	res, err := r.collection.DeleteOne(ctx, bson.M{"_id": id, "orgId": orgID, "system": false})
	if err != nil {
		return false, err
	}
	return res.DeletedCount > 0, nil
}

// EnsureSystemImage upserts a system image
func (r *ImageRepository) EnsureSystemImage(img model.Image) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	img.System = true
	img.CreatedAt = time.Now()

	filter := bson.M{"name": img.Name, "tag": img.Tag, "system": true}
	update := bson.M{"$setOnInsert": img}
	_, err := r.collection.UpdateOne(ctx, filter, update, options.Update().SetUpsert(true))
	return err
}

// Exists checks if an image exists
func (r *ImageRepository) Count(ctx context.Context, orgID primitive.ObjectID, filter interface{}) (int64, error) {
	baseFilter := bson.M{}
	if filterMap, ok := filter.(bson.M); ok {
		for k, v := range filterMap {
			baseFilter[k] = v
		}
	}
	visibilityFilter := bson.M{
		"$or": []bson.M{
			{"orgId": orgID},
			{"system": true},
		},
	}
	finalFilter := visibilityFilter
	if len(baseFilter) > 0 {
		finalFilter = bson.M{
			"$and": []bson.M{baseFilter, visibilityFilter},
		}
	}
	return r.collection.CountDocuments(ctx, finalFilter)
}

func (r *ImageRepository) Exists(ctx context.Context, orgID, id primitive.ObjectID) bool {
	cnt, err := r.Count(ctx, orgID, bson.M{"_id": id})
	return err == nil && cnt > 0
}
