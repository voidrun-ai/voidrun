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

// IOrgRepository defines organization persistence
type IOrgRepository interface {
	Create(ctx context.Context, org *model.Org) (*model.Org, error)
	FindByName(ctx context.Context, name string) (*model.Org, error)
	FindByOwner(ctx context.Context, ownerID primitive.ObjectID) (*model.Org, error)
	FindByID(ctx context.Context, id primitive.ObjectID) (*model.Org, error)
	FindByMember(ctx context.Context, memberID primitive.ObjectID) ([]*model.Org, error)
	FindByClerkOrgID(ctx context.Context, clerkOrgID string) (*model.Org, error)
	Update(ctx context.Context, org *model.Org) error
}

// OrgRepository implements org persistence
type OrgRepository struct {
	cfg        *config.Config
	collection *mongo.Collection
}

func NewOrgRepository(cfg *config.Config, db *mongo.Database) IOrgRepository {
	return &OrgRepository{cfg: cfg, collection: db.Collection("orgs")}
}

func (r *OrgRepository) Create(ctx context.Context, org *model.Org) (*model.Org, error) {
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

func (r *OrgRepository) FindByOwner(ctx context.Context, ownerID primitive.ObjectID) (*model.Org, error) {
	var org *model.Org
	err := r.collection.FindOne(ctx, bson.M{"ownerId": ownerID}).Decode(&org)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return org, nil
}

func (r *OrgRepository) FindByName(ctx context.Context, name string) (*model.Org, error) {
	var org *model.Org
	err := r.collection.FindOne(ctx, bson.M{"name": name}).Decode(&org)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return org, nil
}

func (r *OrgRepository) FindByID(ctx context.Context, id primitive.ObjectID) (*model.Org, error) {
	var org *model.Org
	err := r.collection.FindOne(ctx, bson.M{"_id": id}).Decode(&org)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return org, nil
}

func (r *OrgRepository) FindByMember(ctx context.Context, memberID primitive.ObjectID) ([]*model.Org, error) {
	cursor, err := r.collection.Find(ctx, bson.M{"members": memberID}, options.Find().SetSort(bson.M{"_id": -1}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var orgs []*model.Org
	if err = cursor.All(ctx, &orgs); err != nil {
		return nil, err
	}
	return orgs, nil
}

// FindByClerkOrgID finds an organization by its Clerk organization ID
func (r *OrgRepository) FindByClerkOrgID(ctx context.Context, clerkOrgID string) (*model.Org, error) {
	var org *model.Org
	err := r.collection.FindOne(ctx, bson.M{"clerkOrgId": clerkOrgID}).Decode(&org)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return org, nil
}

// Update updates an existing organization
func (r *OrgRepository) Update(ctx context.Context, org *model.Org) error {
	org.UpdatedAt = time.Now()
	filter := bson.M{"_id": org.ID}
	update := bson.M{"$set": bson.M{
		"name":      org.Name,
		"ownerId":   org.OwnerID,
		"members":   org.Members,
		"plan":      org.Plan,
		"updatedAt": org.UpdatedAt,
	}}
	_, err := r.collection.UpdateOne(ctx, filter, update)
	return err
}
