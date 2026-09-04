package repository

import (
	"context"
	"fmt"
	"time"

	"voidrun/model"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// IEventRepository defines the event storage contract.
type IEventRepository interface {
	SaveEvent(ctx context.Context, event *model.SandboxEvent) error
	SaveEvents(ctx context.Context, events []*model.SandboxEvent) error
	ListBySandbox(ctx context.Context, sandboxID primitive.ObjectID, limit int) ([]*model.SandboxEvent, error)
}

// EventRepository persists sandbox lifecycle events to MongoDB.
type EventRepository struct {
	collection *mongo.Collection
}

// NewEventRepository creates an EventRepository and ensures the required index exists.
func NewEventRepository(db *mongo.Database) *EventRepository {
	r := &EventRepository{
		collection: db.Collection("sandbox_events"),
	}
	r.ensureIndex()
	return r
}

func (r *EventRepository) ensureIndex() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	idx := mongo.IndexModel{
		Keys: bson.D{
			{Key: "sandboxId", Value: 1},
			{Key: "timestamp", Value: -1},
		},
		Options: options.Index().SetUnique(false),
	}
	if _, err := r.collection.Indexes().CreateOne(ctx, idx); err != nil {
		fmt.Printf("[warn] event_repository: failed to create index: %v\n", err)
	}
}

// SaveEvent inserts a single event.
func (r *EventRepository) SaveEvent(ctx context.Context, event *model.SandboxEvent) error {
	if event.ID.IsZero() {
		event.ID = primitive.NewObjectID()
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	_, err := r.collection.InsertOne(ctx, event)
	return err
}

// SaveEvents bulk-inserts a slice of events. Unordered so one bad doc doesn't block the rest.
func (r *EventRepository) SaveEvents(ctx context.Context, events []*model.SandboxEvent) error {
	if len(events) == 0 {
		return nil
	}
	docs := make([]interface{}, 0, len(events))
	now := time.Now()
	for _, ev := range events {
		if ev.ID.IsZero() {
			ev.ID = primitive.NewObjectID()
		}
		if ev.Timestamp.IsZero() {
			ev.Timestamp = now
		}
		docs = append(docs, ev)
	}
	opts := options.InsertMany().SetOrdered(false)
	_, err := r.collection.InsertMany(ctx, docs, opts)
	return err
}

// ListBySandbox returns the most recent events for a sandbox, newest first.
func (r *EventRepository) ListBySandbox(ctx context.Context, sandboxID primitive.ObjectID, limit int) ([]*model.SandboxEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	findOpts := options.Find().
		SetSort(bson.D{{Key: "timestamp", Value: -1}}).
		SetLimit(int64(limit))

	cursor, err := r.collection.Find(ctx, bson.M{"sandboxId": sandboxID}, findOpts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var events []*model.SandboxEvent
	if err := cursor.All(ctx, &events); err != nil {
		return nil, err
	}
	return events, nil
}
