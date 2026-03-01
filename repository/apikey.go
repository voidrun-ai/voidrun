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

type IAPIKeyRepository interface {
	Create(ctx context.Context, orgID primitive.ObjectID, apiKey *model.APIKey) (*model.APIKey, error)
	FindByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID) (*model.APIKey, error)
	FindByHash(ctx context.Context, hash string) (*model.APIKey, error)
	FindByOrgID(ctx context.Context, orgID primitive.ObjectID) ([]*model.APIKey, error)
	FindActive(ctx context.Context) ([]*model.APIKey, error)
	DeleteByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID) (bool, error)
	UpdateByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID, update interface{}) (bool, error)
	UpdateLastUsed(ctx context.Context, id, orgID primitive.ObjectID) error
	Count(ctx context.Context, orgID primitive.ObjectID, filter interface{}) (int64, error)
	Exists(ctx context.Context, orgID primitive.ObjectID, id string) bool
}

// APIKeyRepository manages API keys in MongoDB
type APIKeyRepository struct {
	cfg        *config.Config
	collection *mongo.Collection
}

func NewAPIKeyRepository(cfg *config.Config, db *mongo.Database) IAPIKeyRepository {
	return &APIKeyRepository{
		cfg:        cfg,
		collection: db.Collection("apikeys"),
	}
}

// Create creates a new API key
func (r *APIKeyRepository) Create(ctx context.Context, orgID primitive.ObjectID, apiKey *model.APIKey) (*model.APIKey, error) {
	apiKey.OrgID = orgID
	apiKey.CreatedAt = time.Now()
	apiKey.UpdatedAt = time.Now()
	apiKey.IsActive = true

	result, err := r.collection.InsertOne(ctx, apiKey)
	if err != nil {
		return nil, err
	}
	apiKey.ID = result.InsertedID.(primitive.ObjectID)
	return apiKey, nil
}

// FindByIDAndOrg retrieves an API key by ID and organization ID.
func (r *APIKeyRepository) FindByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID) (*model.APIKey, error) {
	var apiKey *model.APIKey
	err := r.collection.FindOne(ctx, bson.M{"_id": id, "orgId": orgID}).Decode(&apiKey)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return apiKey, nil
}

// FindByHash retrieves an API key by its hash
func (r *APIKeyRepository) FindByHash(ctx context.Context, hash string) (*model.APIKey, error) {
	var apiKey *model.APIKey
	err := r.collection.FindOne(ctx, bson.M{"hash": hash, "isActive": true}).Decode(&apiKey)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return apiKey, nil
}

// FindByOrgID retrieves all active API keys for an organization
func (r *APIKeyRepository) FindByOrgID(ctx context.Context, orgID primitive.ObjectID) ([]*model.APIKey, error) {
	cursor, err := r.collection.Find(ctx, bson.M{"orgId": orgID}, options.Find().SetSort(bson.M{"_id": -1}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var apiKeys []*model.APIKey
	if err = cursor.All(ctx, &apiKeys); err != nil {
		return nil, err
	}
	return apiKeys, nil
}

// FindActive retrieves all active API keys
func (r *APIKeyRepository) FindActive(ctx context.Context) ([]*model.APIKey, error) {
	cursor, err := r.collection.Find(ctx, bson.M{"isActive": true}, options.Find().SetSort(bson.M{"_id": -1}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var apiKeys []*model.APIKey
	if err = cursor.All(ctx, &apiKeys); err != nil {
		return nil, err
	}
	return apiKeys, nil
}

// DeleteByIDAndOrg removes an API key by ID and org ownership.
func (r *APIKeyRepository) DeleteByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID) (bool, error) {
	res, err := r.collection.DeleteOne(ctx, bson.M{"_id": id, "orgId": orgID})
	if err != nil {
		return false, err
	}
	return res.DeletedCount > 0, nil
}

// UpdateByIDAndOrg updates an API key by ID and org ownership.
func (r *APIKeyRepository) UpdateByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID, update interface{}) (bool, error) {
	updateDoc := bson.M{
		"$set": bson.M{
			"updatedAt": time.Now(),
		},
	}

	if updateMap, ok := update.(bson.M); ok {
		for k, v := range updateMap {
			updateDoc["$set"].(bson.M)[k] = v
		}
	} else if updateMap, ok := update.(map[string]interface{}); ok {
		for k, v := range updateMap {
			updateDoc["$set"].(bson.M)[k] = v
		}
	}

	res, err := r.collection.UpdateOne(ctx, bson.M{"_id": id, "orgId": orgID}, updateDoc)
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

// UpdateLastUsed updates the LastUsedAt timestamp
func (r *APIKeyRepository) UpdateLastUsed(ctx context.Context, id, orgID primitive.ObjectID) error {
	_, err := r.collection.UpdateOne(ctx, bson.M{"_id": id, "orgId": orgID}, bson.M{
		"$set": bson.M{
			"lastUsedAt": time.Now(),
			"updatedAt":  time.Now(),
		},
	})
	return err
}

// Count returns the number of API keys matching a filter
func (r *APIKeyRepository) Count(ctx context.Context, orgID primitive.ObjectID, filter interface{}) (int64, error) {
	baseFilter := bson.M{"orgId": orgID}
	if filterMap, ok := filter.(bson.M); ok {
		for k, v := range filterMap {
			baseFilter[k] = v
		}
	}
	baseFilter["orgId"] = orgID
	return r.collection.CountDocuments(ctx, baseFilter)
}

// Exists checks if an API key exists
func (r *APIKeyRepository) Exists(ctx context.Context, orgID primitive.ObjectID, id string) bool {
	objID, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return false
	}
	cnt, err := r.Count(ctx, orgID, bson.M{"_id": objID})
	return err == nil && cnt > 0
}
