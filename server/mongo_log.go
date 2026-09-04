package server

import (
	"context"
	"log"

	"voidrun/config"

	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const mongoQueryLogLimit = 2048

func mongoClientOptions(cfg *config.Config) *options.ClientOptions {
	opts := options.Client().ApplyURI(cfg.Mongo.URI)
	if cfg.Mongo.LogQueries {
		log.Println("[mongo] query logging enabled (MONGO_LOG_QUERIES)")
		opts.SetMonitor(mongoQueryMonitor())
	}
	return opts
}

func mongoQueryMonitor() *event.CommandMonitor {
	return &event.CommandMonitor{
		Started: func(_ context.Context, e *event.CommandStartedEvent) {
			log.Printf("[mongo] %s %s %s", e.DatabaseName, e.CommandName, truncateMongoCmd(e.Command.String(), mongoQueryLogLimit))
		},
		Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
			log.Printf("[mongo] ok %s %s %s", e.DatabaseName, e.CommandName, e.Duration)
		},
		Failed: func(_ context.Context, e *event.CommandFailedEvent) {
			log.Printf("[mongo] fail %s %s %s: %s", e.DatabaseName, e.CommandName, e.Duration, e.Failure)
		},
	}
}

func truncateMongoCmd(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
