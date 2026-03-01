package repository

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"voidrun/internal/config"
	"voidrun/internal/model"
	"voidrun/pkg/timer"
	"voidrun/pkg/util"

	"github.com/3th1nk/cidr"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type ISandboxRepository interface {
	Create(ctx context.Context, sandbox *model.Sandbox) error
	FindByID(ctx context.Context, id string) (*model.Sandbox, error)
	Find(ctx context.Context, filter interface{}, opts options.FindOptions) ([]*model.Sandbox, error)
	Delete(ctx context.Context, id primitive.ObjectID) error
	UpdateStatus(ctx context.Context, id primitive.ObjectID, status string) error
	Count(ctx context.Context, filter interface{}) (int64, error)
	Exists(ctx context.Context, id string) bool
	NextAvailableIP() (string, error)
}

// SandboxRepository handles sandbox persistence in MongoDB
type SandboxRepository struct {
	instancesDir string
	networkCIDR  string
	mu           sync.Mutex
	cfg          *config.Config
	collection   *mongo.Collection
	allocatedIPs map[string]bool // Cache of all allocated IPs
}

// NewSandboxRepository creates a new sandbox repository
func NewSandboxRepository(cfg *config.Config, db *mongo.Database) *SandboxRepository {
	return &SandboxRepository{
		instancesDir: cfg.Paths.InstancesDir,
		networkCIDR:  cfg.Network.NetworkCIDR,
		cfg:          cfg,
		collection:   db.Collection("sandboxes"),
		allocatedIPs: make(map[string]bool),
	}
}

// Init initializes the repository by loading all allocated IPs from the database
func (r *SandboxRepository) Init(ctx context.Context) error {
	// Create index on orgId for faster list queries
	indexOpts := options.Index().SetUnique(false)
	indexModel := mongo.IndexModel{
		Keys:    bson.D{bson.E{Key: "orgId", Value: 1}},
		Options: indexOpts,
	}
	if _, err := r.collection.Indexes().CreateOne(ctx, indexModel); err != nil {
		fmt.Printf("[warn] failed to create orgId index: %v\n", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	cur, err := r.collection.Find(ctx, bson.M{"ip": bson.M{"$ne": ""}}, &options.FindOptions{
		Projection: bson.M{"ip": 1},
	})
	if err != nil {
		return fmt.Errorf("failed to fetch allocated IPs: %w", err)
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var doc struct {
			IP string `bson:"ip"`
		}
		if err := cur.Decode(&doc); err != nil {
			return fmt.Errorf("failed to decode IP: %w", err)
		}
		if doc.IP != "" {
			r.allocatedIPs[doc.IP] = true
		}
	}

	if err := cur.Err(); err != nil {
		return fmt.Errorf("cursor error: %w", err)
	}

	return nil
}

// NextAvailableIP returns a random available IP from the CIDR range
func (r *SandboxRepository) NextAvailableIP() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer timer.Track("NextAvailableIP (Total)")()

	// Parse CIDR notation
	c, err := cidr.Parse(r.networkCIDR)
	if err != nil {
		return "", fmt.Errorf("invalid CIDR notation: %w", err)
	}

	// Get total IP count in the range (excluding network and broadcast)
	totalIPs := int(c.IPCount().Int64())
	if totalIPs < 2 {
		return "", fmt.Errorf("CIDR range too small: %s", r.networkCIDR)
	}

	// Try to find a random available IP (max 100 attempts)
	for attempts := 0; attempts < 100; attempts++ {
		// Get random index within available IPs
		randomIndex := rand.Intn(totalIPs)

		var selectedIP string
		count := 0

		// Iterate through IPs and find the random one
		c.Each(func(ip string) bool {
			if count == randomIndex {
				if ip != "" && !r.allocatedIPs[ip] {
					selectedIP = ip
					r.allocatedIPs[selectedIP] = true
					return false // Stop iteration
				}
			}
			count++
			return true
		})

		if selectedIP != "" {
			return selectedIP, nil
		}
	}

	return "", fmt.Errorf("no free IPs available in subnet %s", r.networkCIDR)
}

func (r *SandboxRepository) Create(ctx context.Context, sandbox *model.Sandbox) error {
	defer timer.Track("SandboxRepository.Create Mongo (Total)")()

	sandbox.CreatedAt = time.Now()
	result, err := r.collection.InsertOne(ctx, sandbox)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("sandbox id %s already exists", sandbox.ID)
		}
		return err
	}
	sandbox.ID = result.InsertedID.(primitive.ObjectID)
	return nil
}

func (r *SandboxRepository) FindByID(ctx context.Context, id string) (*model.Sandbox, error) {
	oid, err := util.ParseObjectID(id)
	if err != nil {
		return nil, err
	}

	var sandbox *model.Sandbox
	err = r.collection.FindOne(ctx, bson.M{"_id": oid}).Decode(&sandbox)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return sandbox, nil
}

func (r *SandboxRepository) Find(ctx context.Context, filter interface{}, opts options.FindOptions) ([]*model.Sandbox, error) {
	cursor, err := r.collection.Find(ctx, filter, &opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var sandboxes []*model.Sandbox
	if err = cursor.All(ctx, &sandboxes); err != nil {
		return nil, err
	}
	return sandboxes, nil
}

func (r *SandboxRepository) Delete(ctx context.Context, id primitive.ObjectID) error {
	_, err := r.collection.DeleteOne(ctx, bson.M{"_id": id})
	return err
}

func (r *SandboxRepository) UpdateStatus(ctx context.Context, id primitive.ObjectID, status string) error {
	_, err := r.collection.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{
		"status":    status,
		"updatedAt": time.Now(),
	}})
	return err
}

func (r *SandboxRepository) Count(ctx context.Context, filter interface{}) (int64, error) {
	count, err := r.collection.CountDocuments(ctx, filter)
	return count, err
}

func (r *SandboxRepository) Exists(ctx context.Context, id string) bool {
	objID, err := util.ParseObjectID(id)
	if err != nil {
		return false
	}
	count, err := r.Count(ctx, bson.M{"_id": objID})
	if err != nil {
		return false
	}
	return count > 0
}
