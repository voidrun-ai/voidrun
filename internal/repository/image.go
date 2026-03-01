package repository

import (
	"context"
	"time"

	"voidrun/internal/config"
	"voidrun/internal/model"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type IImageRepository interface {
	Create(ctx context.Context, image *model.Image) (*model.Image, error)
	FindByID(ctx context.Context, id string) (*model.Image, error)
	Find(ctx context.Context, filter interface{}, opts options.FindOptions) ([]*model.Image, error)
	Delete(ctx context.Context, id string) error
	Count(ctx context.Context, filter interface{}) (int64, error)
	Exists(ctx context.Context, id string) bool

	GetLatestByName(name string) (*model.Image, error)
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
func (r *ImageRepository) Create(ctx context.Context, img *model.Image) (*model.Image, error) {
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

// Get retrieves an image by ID
func (r *ImageRepository) FindByID(ctx context.Context, id string) (*model.Image, error) {
	var img *model.Image
	err := r.collection.FindOne(ctx, bson.M{"_id": id}).Decode(&img)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return img, nil
}

// GetByNameTag retrieves an image by name and tag
func (r *ImageRepository) GetLatestByName(name string) (*model.Image, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var img *model.Image
	err := r.collection.FindOne(ctx, bson.M{"name": name}, options.FindOne().SetSort(bson.D{{Key: "_id", Value: -1}})).Decode(&img)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return img, nil
}

// GetAll retrieves all images
func (r *ImageRepository) Find(ctx context.Context, filter interface{}, opts options.FindOptions) ([]*model.Image, error) {
	if filter == nil {
		filter = bson.M{}
	}
	cursor, err := r.collection.Find(ctx, filter, &opts)
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
func (r *ImageRepository) Delete(ctx context.Context, id string) error {
	_, err := r.collection.DeleteOne(ctx, bson.M{"_id": id})
	return err
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
func (r *ImageRepository) Count(ctx context.Context, filter interface{}) (int64, error) {
	return r.collection.CountDocuments(ctx, filter)
}

func (r *ImageRepository) Exists(ctx context.Context, id string) bool {
	cnt, err := r.Count(ctx, bson.M{"_id": id})
	return err == nil && cnt > 0
}
