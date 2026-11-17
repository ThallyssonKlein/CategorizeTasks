// Package usage stores one document per classification in MongoDB, so token
// consumption can be summed and projected (for example per month).
package usage

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Record is one classification call.
type Record struct {
	// EventID is the Notion event ID and doubles as the document _id, so a
	// redelivered event overwrites its own record instead of duplicating it.
	EventID          string    `bson:"_id"`
	Time             time.Time `bson:"ts"`
	PageID           string    `bson:"page_id"`
	Model            string    `bson:"model"`
	InputTokens      int64     `bson:"input_tokens"`
	OutputTokens     int64     `bson:"output_tokens"`
	CacheReadTokens  int64     `bson:"cache_read_tokens"`
	CacheWriteTokens int64     `bson:"cache_write_tokens"`
	Category         string    `bson:"category"`
	DryRun           bool      `bson:"dry_run"`
	// Estimated marks records where the model was not run: input tokens are
	// exact (count_tokens), output tokens are an estimate.
	Estimated bool `bson:"estimated"`
}

// Recorder persists usage records.
type Recorder interface {
	Record(ctx context.Context, r Record) error
}

// Mongo writes records to a MongoDB collection.
type Mongo struct {
	coll *mongo.Collection
}

// NewMongoFromEnv returns a recorder when MONGODB_URI is set, or nil when it
// is not (usage tracking is optional). MONGODB_DB defaults to "categorize" and
// MONGODB_COLLECTION to "usage". The client connects lazily, so this does not
// block on the network.
func NewMongoFromEnv() (*Mongo, error) {
	uri := os.Getenv("MONGODB_URI")
	if uri == "" {
		return nil, nil
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("usage: mongo connect: %w", err)
	}
	return &Mongo{coll: client.Database(envOr("MONGODB_DB", "categorize")).
		Collection(envOr("MONGODB_COLLECTION", "usage"))}, nil
}

func (m *Mongo) Record(ctx context.Context, r Record) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := m.coll.ReplaceOne(ctx, bson.M{"_id": r.EventID}, r, options.Replace().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("usage: save: %w", err)
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
