package service

import (
	"reflect"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestSandboxListFilterUsesLabelsDirectly(t *testing.T) {
	orgID := primitive.NewObjectID()
	got := sandboxListFilter(orgID, map[string]string{
		"env":  "prod",
		"team": "api",
	})
	want := bson.M{
		"orgId":       orgID,
		"labels.env":  "prod",
		"labels.team": "api",
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected filter: got %#v, want %#v", got, want)
	}
	if _, exists := got["labelPairs"]; exists {
		t.Fatal("labelPairs must not be used for filtering")
	}
}
