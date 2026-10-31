package repository

import (
	"context"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func creatingTestCollection(t *testing.T) *mongo.Collection {
	t.Helper()
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("MONGO_TEST_URI not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri).SetServerSelectionTimeout(2*time.Second))
	if err != nil {
		t.Skip("mongo not available")
	}
	db := client.Database("voidrun_sandbox_creating_test")
	if err := db.Drop(ctx); err != nil {
		t.Skip("mongo not available")
	}
	t.Cleanup(func() {
		_ = db.Drop(context.Background())
		_ = client.Disconnect(context.Background())
	})
	return db.Collection("sandboxes")
}

func TestCommitCreateOnlyFromCreating(t *testing.T) {
	col := creatingTestCollection(t)
	r := &SandboxRepository{collection: col}
	ctx := context.Background()
	orgID := primitive.NewObjectID()
	creating := primitive.NewObjectID()
	deleted := primitive.NewObjectID()
	if _, err := col.InsertMany(ctx, []any{
		bson.M{"_id": creating, "orgId": orgID, "status": "creating"},
		bson.M{"_id": deleted, "orgId": orgID, "status": "deleted"},
	}); err != nil {
		t.Fatal(err)
	}

	ok, err := r.CommitCreate(ctx, creating, orgID, bson.M{"status": "running", "ip": "10.0.0.2"})
	if err != nil || !ok {
		t.Fatalf("commit creating row: ok=%v err=%v", ok, err)
	}
	ok, err = r.CommitCreate(ctx, deleted, orgID, bson.M{"status": "running"})
	if err != nil || ok {
		t.Fatalf("commit deleted row: ok=%v err=%v", ok, err)
	}
	var doc struct {
		Status string `bson:"status"`
	}
	if err := col.FindOne(ctx, bson.M{"_id": deleted}).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.Status != "deleted" {
		t.Fatalf("deleted row status = %q", doc.Status)
	}
}

func TestMarkDeletedOnce(t *testing.T) {
	col := creatingTestCollection(t)
	r := &SandboxRepository{collection: col}
	ctx := context.Background()
	orgID := primitive.NewObjectID()
	id := primitive.NewObjectID()
	if _, err := col.InsertOne(ctx, bson.M{"_id": id, "orgId": orgID, "status": "running"}); err != nil {
		t.Fatal(err)
	}
	ok, err := r.MarkDeleted(ctx, id, orgID)
	if err != nil || !ok {
		t.Fatalf("first delete: ok=%v err=%v", ok, err)
	}
	ok, err = r.MarkDeleted(ctx, id, orgID)
	if err != nil || ok {
		t.Fatalf("second delete: ok=%v err=%v", ok, err)
	}
}

func TestDeleteCreatingForNodeScopesToNode(t *testing.T) {
	col := creatingTestCollection(t)
	r := &SandboxRepository{collection: col}
	ctx := context.Background()
	mine := primitive.NewObjectID()
	other := primitive.NewObjectID()
	running := primitive.NewObjectID()
	if _, err := col.InsertMany(ctx, []any{
		bson.M{"_id": mine, "nodeId": "host-a", "status": "creating"},
		bson.M{"_id": other, "nodeId": "host-b", "status": "creating"},
		bson.M{"_id": running, "nodeId": "host-a", "status": "running"},
	}); err != nil {
		t.Fatal(err)
	}

	ids, err := r.ListCreatingForNode(ctx, "host-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0].ID != mine {
		t.Fatalf("ids = %v", ids)
	}
	if err := r.DeleteStillCreating(ctx, []primitive.ObjectID{mine}); err != nil {
		t.Fatal(err)
	}
	n, err := col.CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("remaining rows = %d, want 2", n)
	}
	if ids, err := r.ListCreatingForNode(ctx, ""); err != nil || ids != nil {
		t.Fatalf("empty node: ids=%v err=%v", ids, err)
	}
}
