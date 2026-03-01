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

type IUserRepository interface {
	Create(ctx context.Context, user *model.User) error
	FindByID(ctx context.Context, id primitive.ObjectID) (*model.User, error)
	Find(ctx context.Context, filter interface{}, opts options.FindOptions) ([]*model.User, error)
	Delete(ctx context.Context, id primitive.ObjectID) error
	Count(ctx context.Context, filter interface{}) (int64, error)
	Exists(ctx context.Context, id string) bool

	// Add only specific methods here
	FindByEmail(ctx context.Context, email string) (*model.User, error)
	EnsureSystemUser(u model.User) error
	FindByIDs(ctx context.Context, ids []primitive.ObjectID) ([]*model.User, error)
}

type UserRepository struct {
	cfg        *config.Config
	collection *mongo.Collection
}

func NewUserRepository(cfg *config.Config, db *mongo.Database) IUserRepository {
	return &UserRepository{
		cfg:        cfg,
		collection: db.Collection("users"),
	}
}

func (r *UserRepository) Create(ctx context.Context, user *model.User) error {
	user.CreatedAt = time.Now()
	result, err := r.collection.InsertOne(ctx, user)
	if err != nil {
		return err
	}
	user.ID = result.InsertedID.(primitive.ObjectID)
	return nil
}

func (r *UserRepository) FindByID(ctx context.Context, id primitive.ObjectID) (*model.User, error) {
	var user *model.User
	err := r.collection.FindOne(ctx, bson.M{"_id": id}).Decode(&user)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return user, nil
}

func (r *UserRepository) Find(ctx context.Context, filter interface{}, opts options.FindOptions) ([]*model.User, error) {
	cursor, err := r.collection.Find(ctx, filter, &opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var users []*model.User
	if err = cursor.All(ctx, &users); err != nil {
		return nil, err
	}
	return users, nil
}

func (r *UserRepository) Delete(ctx context.Context, id primitive.ObjectID) error {
	_, err := r.collection.DeleteOne(ctx, bson.M{"_id": id})
	return err
}

func (r *UserRepository) FindByEmail(ctx context.Context, email string) (*model.User, error) {
	var user *model.User
	err := r.collection.FindOne(ctx, bson.M{"email": email}).Decode(&user)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return user, nil
}

func (r *UserRepository) EnsureSystemUser(u model.User) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	u.System = true
	u.Role = "system"
	u.CreatedAt = time.Now()

	filter := bson.M{"email": u.Email, "system": true}
	update := bson.M{"$setOnInsert": u}
	_, err := r.collection.UpdateOne(ctx, filter, update, options.Update().SetUpsert(true))
	return err
}

func (r *UserRepository) Count(ctx context.Context, filter interface{}) (int64, error) {
	return r.collection.CountDocuments(ctx, filter)
}

func (r *UserRepository) Exists(ctx context.Context, id string) bool {
	oid, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return false
	}
	cnt, err := r.Count(ctx, bson.M{"_id": oid})
	return err == nil && cnt > 0
}

// FindByIDs returns users matching the given object IDs
func (r *UserRepository) FindByIDs(ctx context.Context, ids []primitive.ObjectID) ([]*model.User, error) {
	if len(ids) == 0 {
		return []*model.User{}, nil
	}
	filter := bson.M{"_id": bson.M{"$in": ids}}
	cursor, err := r.collection.Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var users []*model.User
	if err = cursor.All(ctx, &users); err != nil {
		return nil, err
	}
	return users, nil
}
