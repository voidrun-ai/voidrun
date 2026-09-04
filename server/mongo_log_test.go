package server

import (
	"strings"
	"testing"

	"voidrun/config"
)

func TestMongoClientOptions_MonitorOffByDefault(t *testing.T) {
	opts := mongoClientOptions(&config.Config{Mongo: config.MongoConfig{URI: "mongodb://localhost:27017"}})
	if opts.Monitor != nil {
		t.Fatal("monitor must be nil unless MONGO_LOG_QUERIES is set")
	}
}

func TestMongoClientOptions_MonitorOn(t *testing.T) {
	opts := mongoClientOptions(&config.Config{Mongo: config.MongoConfig{
		URI:        "mongodb://localhost:27017",
		LogQueries: true,
	}})
	if opts.Monitor == nil {
		t.Fatal("monitor must be set when LogQueries is true")
	}
}

func TestTruncateMongoCmd(t *testing.T) {
	if got := truncateMongoCmd("abc", 10); got != "abc" {
		t.Fatalf("got %q", got)
	}
	got := truncateMongoCmd(strings.Repeat("x", 10), 4)
	if got != "xxxx...(truncated)" {
		t.Fatalf("got %q", got)
	}
}
