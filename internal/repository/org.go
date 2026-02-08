package repository

import (
	"context"
	"time"

	"voidrun/internal/config"
	"voidrun/internal/model"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// IOrgRepository defines organization persistence
type IOrgRepository interface {
	Create(ctx context.Context, org *model.Organization) (*model.Organization, error)
	FindByOwner(ctx context.Context, ownerID primitive.ObjectID) (*model.Organization, error)
	FindByID(ctx context.Context, id primitive.ObjectID) (*model.Organization, error)
}

// OrgRepository implements org persistence
type OrgRepository struct {
	cfg        *config.Config
	collection *mongo.Collection
}

func NewOrgRepository(cfg *config.Config, db *mongo.Database) IOrgRepository {
	return &OrgRepository{cfg: cfg, collection: db.Collection("organizations")}
}

func (r *OrgRepository) Create(ctx context.Context, org *model.Organization) (*model.Organization, error) {
	now := time.Now()
	org.CreatedAt = now
	org.UpdatedAt = now
	res, err := r.collection.InsertOne(ctx, org)
	if err != nil {
		return nil, err
	}
	if oid, ok := res.InsertedID.(primitive.ObjectID); ok {
		org.ID = oid
	}
	return org, nil
}

func (r *OrgRepository) FindByOwner(ctx context.Context, ownerID primitive.ObjectID) (*model.Organization, error) {
	var org *model.Organization
	err := r.collection.FindOne(ctx, bson.M{"ownerId": ownerID}).Decode(&org)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return org, nil
}

func (r *OrgRepository) FindByID(ctx context.Context, id primitive.ObjectID) (*model.Organization, error) {
	var org *model.Organization
	err := r.collection.FindOne(ctx, bson.M{"_id": id}).Decode(&org)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return org, nil
}
