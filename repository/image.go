package repository

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"voidrun/config"
	"voidrun/model"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type IImageRepository interface {
	Create(ctx context.Context, image *model.Image) (*model.Image, error)
	FindByIDAndOrgOrSystem(ctx context.Context, id, orgID primitive.ObjectID) (*model.Image, error)
	Find(ctx context.Context, orgID primitive.ObjectID, filter interface{}, opts options.FindOptions) ([]*model.Image, error)
	DeleteByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID) (bool, error)
	Count(ctx context.Context, orgID primitive.ObjectID, filter interface{}) (int64, error)
	Exists(ctx context.Context, orgID, id primitive.ObjectID) bool

	GetLatestByNameForOrg(name string, orgID primitive.ObjectID) (*model.Image, error)
	EnsureSystemImage(img model.Image) error
	DeactivateStaleSystemImages(ctx context.Context, validImages []model.Image) error
	ResolveImage(ctx context.Context, orgID primitive.ObjectID, imageSpec string) (*model.Image, error)
}

// ImageRepository manages images in MongoDB
type ImageRepository struct {
	cfg          *config.Config
	collection   *mongo.Collection
	resolveCache sync.Map // key: "orgHex:imageSpec" → imageCacheEntry
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

	// If this image is being set as active, deactivate all other system images with the same name
	if img.Active {
		_, err := r.collection.UpdateMany(ctx,
			bson.M{"name": img.Name, "system": true, "tag": bson.M{"$ne": img.Tag}},
			bson.M{"$set": bson.M{"active": false}},
		)
		if err != nil {
			return fmt.Errorf("failed to deactivate old system images: %w", err)
		}
	}

	filter := bson.M{"name": img.Name, "tag": img.Tag, "system": true}
	update := bson.M{
		"$setOnInsert": bson.M{
			"_id":       primitive.NewObjectID(),
			"name":      img.Name,
			"tag":       img.Tag,
			"system":    true,
			"createdAt": time.Now(),
		},
		"$set": bson.M{
			"sizeGB":      img.SizeGB,
			"active":      img.Active,
			"description": img.Description,
		},
	}
	_, err := r.collection.UpdateOne(ctx, filter, update, options.Update().SetUpsert(true))
	return err
}

func (r *ImageRepository) DeactivateStaleSystemImages(ctx context.Context, validImages []model.Image) error {
	names := make([]string, 0)
	nameSet := make(map[string]bool)
	for _, img := range validImages {
		if !nameSet[img.Name] {
			names = append(names, img.Name)
			nameSet[img.Name] = true
		}
	}

	_, err := r.collection.UpdateMany(ctx,
		bson.M{"system": true, "name": bson.M{"$nin": names}},
		bson.M{"$set": bson.M{"active": false}},
	)
	if err != nil {
		return err
	}

	cursor, err := r.collection.Find(ctx, bson.M{"system": true, "name": bson.M{"$in": names}})
	if err != nil {
		return err
	}
	defer cursor.Close(ctx)

	var inDB []model.Image
	if err := cursor.All(ctx, &inDB); err != nil {
		return err
	}

	validMap := make(map[string]bool)
	for _, img := range validImages {
		validMap[fmt.Sprintf("%s:%s", img.Name, img.Tag)] = true
	}

	for _, dbImg := range inDB {
		key := fmt.Sprintf("%s:%s", dbImg.Name, dbImg.Tag)
		if !validMap[key] && dbImg.Active {
			_, _ = r.collection.UpdateOne(ctx, bson.M{"_id": dbImg.ID}, bson.M{"$set": bson.M{"active": false}})
		}
	}

	return nil
}

func extractNames(imgs []model.Image) []string {
	names := make([]string, 0)
	set := make(map[string]bool)
	for _, img := range imgs {
		if !set[img.Name] {
			names = append(names, img.Name)
			set[img.Name] = true
		}
	}
	return names
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

func (r *ImageRepository) ResolveImage(ctx context.Context, orgID primitive.ObjectID, imageSpec string) (*model.Image, error) {
	cacheKey := orgID.Hex() + ":" + imageSpec

	// Check cache first
	if cached, ok := r.resolveCache.Load(cacheKey); ok {
		return cached.(*model.Image), nil
	}

	name, tag := imageSpec, ""
	if idx := strings.Index(imageSpec, ":"); idx != -1 {
		name = imageSpec[:idx]
		tag = imageSpec[idx+1:]
	}

	filter := bson.M{
		"name": name,
		"$or": []bson.M{
			{"orgId": orgID},
			{"system": true},
		},
	}

	if tag != "" {
		filter["tag"] = tag
	} else {
		// If no tag is provided, prefer the active version
		filter["active"] = true
	}

	var img model.Image
	err := r.collection.FindOne(ctx, filter, options.FindOne().SetSort(bson.D{{Key: "createdAt", Value: -1}})).Decode(&img)
	if err != nil {
		if err == mongo.ErrNoDocuments && tag == "" {
			// Fallback: if no active one found, get the absolute latest one by ID (lexicographical/time)
			delete(filter, "active")
			err = r.collection.FindOne(ctx, filter, options.FindOne().SetSort(bson.D{{Key: "_id", Value: -1}})).Decode(&img)
		}

		if err != nil {
			if err == mongo.ErrNoDocuments {
				return nil, fmt.Errorf("image %q not found", imageSpec)
			}
			return nil, err
		}
	}

	// Only cache system images — they change very rarely.
	// Org-specific images are always re-resolved from the DB.
	if img.System {
		r.resolveCache.Store(cacheKey, &img)
	}

	return &img, nil
}
