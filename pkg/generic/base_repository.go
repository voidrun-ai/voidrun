package generic

import (
	"context"
	"errors"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// BaseRepository Interface
type BaseRepository[T Entity] interface {
	Create(ctx context.Context, entity T) error
	GetByID(ctx context.Context, id string) (T, error)
	Update(ctx context.Context, entity T) error
	Delete(ctx context.Context, id string) error
}

// MongoBaseRepository Implementation
type MongoBaseRepository[T Entity] struct {
	Collection *mongo.Collection
}

func NewBaseRepository[T Entity](collection *mongo.Collection) *MongoBaseRepository[T] {
	return &MongoBaseRepository[T]{Collection: collection}
}

// 1. Create
func (r *MongoBaseRepository[T]) Create(ctx context.Context, entity T) error {
	entity.SetID(primitive.NewObjectID())
	_, err := r.Collection.InsertOne(ctx, entity)
	return err
}

// 2. GetByID
func (r *MongoBaseRepository[T]) GetByID(ctx context.Context, id string) (T, error) {
	var entity T
	objID, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return entity, errors.New("invalid id")
	}

	err = r.Collection.FindOne(ctx, bson.M{"_id": objID}).Decode(&entity)
	return entity, err
}

// 3. Update (Full Replace)
func (r *MongoBaseRepository[T]) Update(ctx context.Context, entity T) error {
	_, err := r.Collection.ReplaceOne(ctx, bson.M{"_id": entity.GetID()}, entity)
	return err
}

// 4. Delete
func (r *MongoBaseRepository[T]) Delete(ctx context.Context, id string) error {
	objID, _ := primitive.ObjectIDFromHex(id)
	_, err := r.Collection.DeleteOne(ctx, bson.M{"_id": objID})
	return err
}
