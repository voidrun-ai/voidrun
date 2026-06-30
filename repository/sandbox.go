package repository

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"voidrun/config"
	"voidrun/model"
	"voidrun/util"

	"github.com/3th1nk/cidr"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type ISandboxRepository interface {
	Create(ctx context.Context, sandbox *model.Sandbox) error
	FindByIDAndOrg(ctx context.Context, orgID primitive.ObjectID, id primitive.ObjectID, opts options.FindOneOptions) (*model.Sandbox, error)
	FindAndOrg(ctx context.Context, orgID primitive.ObjectID, filter interface{}, opts options.FindOptions) ([]*model.Sandbox, error)
	DeleteByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID) (bool, error)
	UpdateStatusByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID, status string) (bool, error)
	UpdateTapNameByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID, tapName string) (bool, error)
	UpdateNetNSByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID, tapName, netnsName string) (bool, error)
	Count(ctx context.Context, orgID primitive.ObjectID, filter interface{}) (int64, error)
	Exists(ctx context.Context, orgID primitive.ObjectID, id string) bool
	FindForHealth(ctx context.Context, opts options.FindOptions) ([]*model.Sandbox, error)
	UpdateStatusForHealth(ctx context.Context, id primitive.ObjectID, status string) error
	NextAvailableIP() (string, error)
	// Lifecycle management methods
	TouchActivity(ctx context.Context, id primitive.ObjectID) error
	SetSnapshottedAt(ctx context.Context, id primitive.ObjectID) error
	SetSnapshottedAtAndOrg(ctx context.Context, id, orgID primitive.ObjectID) (bool, error)
	FindIdleRunning(ctx context.Context, threshold time.Time) ([]*model.Sandbox, error)
	FindStaleSnapshotted(ctx context.Context, threshold time.Time) ([]*model.Sandbox, error)
	FindByID(ctx context.Context, id primitive.ObjectID, opts options.FindOneOptions) (*model.Sandbox, error)
	FreeIP(ctx context.Context, ip string)
}

// SandboxRepository handles sandbox persistence in MongoDB
type SandboxRepository struct {
	instancesDir string
	networkCIDR  string
	mu           sync.RWMutex
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
	// Compound indexes turn the auto-lifecycle sweeps into index range scans.
	indexes := []mongo.IndexModel{
		{Keys: bson.D{{Key: "orgId", Value: 1}}, Options: options.Index().SetUnique(false)},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "lastActivityAt", Value: 1}}, Options: options.Index().SetUnique(false)},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "snapshottedAt", Value: 1}}, Options: options.Index().SetUnique(false)},
	}
	if _, err := r.collection.Indexes().CreateMany(ctx, indexes); err != nil {
		fmt.Printf("[warn] failed to create sandbox indexes: %v\n", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	cur, err := r.collection.Find(ctx, bson.M{"ip": bson.M{"$ne": ""}, "status": bson.M{"$nin": []string{"deleted", "killed"}}}, &options.FindOptions{
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
	defer util.Track("NextAvailableIP (Total)")()

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
	defer util.Track("SandboxRepository.Create Mongo (Total)")()

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

func (r *SandboxRepository) FindByIDAndOrg(ctx context.Context, orgID primitive.ObjectID, id primitive.ObjectID, opts options.FindOneOptions) (*model.Sandbox, error) {
	var sandbox *model.Sandbox
	err := r.collection.FindOne(ctx, bson.M{"_id": id, "orgId": orgID}, &opts).Decode(&sandbox)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return sandbox, nil
}

func (r *SandboxRepository) FindAndOrg(ctx context.Context, orgID primitive.ObjectID, filter interface{}, opts options.FindOptions) ([]*model.Sandbox, error) {
	baseFilter := bson.M{"orgId": orgID, "status": bson.M{"$ne": "deleted"}}
	if filterMap, ok := filter.(bson.M); ok {
		for k, v := range filterMap {
			baseFilter[k] = v
		}
	}

	// Never allow caller to override org boundary.
	baseFilter["orgId"] = orgID

	cursor, err := r.collection.Find(ctx, baseFilter, &opts)
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

func (r *SandboxRepository) FindForHealth(ctx context.Context, opts options.FindOptions) ([]*model.Sandbox, error) {
	cursor, err := r.collection.Find(ctx, bson.M{"status": bson.M{"$nin": []string{"killed", "deleted"}}}, &opts)
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

func (r *SandboxRepository) DeleteByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID) (bool, error) {
	res, err := r.collection.DeleteOne(ctx, bson.M{"_id": id, "orgId": orgID})
	if err != nil {
		return false, err
	}
	return res.DeletedCount > 0, nil
}

// UpdateStatusForHealth transitions a "running" row to a new status.
// CAS-guarded so concurrent lifecycle ops are not overwritten.
func (r *SandboxRepository) UpdateStatusForHealth(ctx context.Context, id primitive.ObjectID, status string) error {
	_, err := r.collection.UpdateOne(
		ctx,
		bson.M{"_id": id, "status": "running"},
		bson.M{"$set": bson.M{
			"status":    status,
			"updatedAt": time.Now(),
		}},
	)
	return err
}

func (r *SandboxRepository) UpdateStatusByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID, status string) (bool, error) {
	res, err := r.collection.UpdateOne(ctx, bson.M{"_id": id, "orgId": orgID}, bson.M{"$set": bson.M{
		"status":    status,
		"updatedAt": time.Now(),
	}})
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

func (r *SandboxRepository) UpdateTapNameByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID, tapName string) (bool, error) {
	res, err := r.collection.UpdateOne(ctx, bson.M{"_id": id, "orgId": orgID}, bson.M{"$set": bson.M{
		"tapName":   tapName,
		"updatedAt": time.Now(),
	}})
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

func (r *SandboxRepository) UpdateNetNSByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID, tapName, netnsName string) (bool, error) {
	res, err := r.collection.UpdateOne(ctx, bson.M{"_id": id, "orgId": orgID}, bson.M{"$set": bson.M{
		"tapName":   tapName,
		"netnsName": netnsName,
		"updatedAt": time.Now(),
	}})
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

func (r *SandboxRepository) Count(ctx context.Context, orgID primitive.ObjectID, filter interface{}) (int64, error) {
	baseFilter := bson.M{"orgId": orgID, "status": bson.M{"$ne": "deleted"}}
	if filterMap, ok := filter.(bson.M); ok {
		for k, v := range filterMap {
			baseFilter[k] = v
		}
	}
	baseFilter["orgId"] = orgID

	count, err := r.collection.CountDocuments(ctx, baseFilter)
	return count, err
}

func (r *SandboxRepository) Exists(ctx context.Context, orgID primitive.ObjectID, id string) bool {
	objID, err := util.ParseObjectID(id)
	if err != nil {
		return false
	}
	count, err := r.Count(ctx, orgID, bson.M{"_id": objID})
	if err != nil {
		return false
	}
	return count > 0
}

// TouchActivity updates the lastActivityAt timestamp for a sandbox
func (r *SandboxRepository) TouchActivity(ctx context.Context, id primitive.ObjectID) error {
	_, err := r.collection.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{
		"lastActivityAt": time.Now(),
	}})
	return err
}

// SetSnapshottedAt sets the snapshottedAt timestamp and status to snapshotted
func (r *SandboxRepository) SetSnapshottedAt(ctx context.Context, id primitive.ObjectID) error {
	now := time.Now()
	_, err := r.collection.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{
		"status":        "snapshotted",
		"snapshottedAt": now,
		"updatedAt":     now,
	}})
	return err
}

func (r *SandboxRepository) SetSnapshottedAtAndOrg(ctx context.Context, id, orgID primitive.ObjectID) (bool, error) {
	now := time.Now()
	res, err := r.collection.UpdateOne(ctx, bson.M{"_id": id, "orgId": orgID}, bson.M{"$set": bson.M{
		"status":        "snapshotted",
		"snapshottedAt": now,
		"updatedAt":     now,
	}})
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

// FindIdleRunning finds running sandboxes that have been idle since before the threshold
func (r *SandboxRepository) FindIdleRunning(ctx context.Context, threshold time.Time) ([]*model.Sandbox, error) {
	filter := bson.M{
		"status": "running",
		"$or": []bson.M{
			{"autoSleep": bson.M{"$ne": false}},
			{"autoSleep": bson.M{"$exists": false}},
		},
		"lastActivityAt": bson.M{"$lt": threshold},
	}
	cursor, err := r.collection.Find(ctx, filter, &options.FindOptions{
		Projection: bson.M{"_id": 1, "orgId": 1, "name": 1},
	})
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

// FindStaleSnapshotted finds snapshotted sandboxes that have been snapshotted since before the threshold
func (r *SandboxRepository) FindStaleSnapshotted(ctx context.Context, threshold time.Time) ([]*model.Sandbox, error) {
	filter := bson.M{
		"status":        "snapshotted",
		"snapshottedAt": bson.M{"$lt": threshold},
	}
	cursor, err := r.collection.Find(ctx, filter, &options.FindOptions{
		Projection: bson.M{"_id": 1, "orgId": 1, "name": 1, "createdBy": 1, "tapName": 1, "netnsName": 1},
	})
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

func (r *SandboxRepository) FindByID(ctx context.Context, id primitive.ObjectID, opts options.FindOneOptions) (*model.Sandbox, error) {
	var sandbox *model.Sandbox
	err := r.collection.FindOne(ctx, bson.M{"_id": id}, &opts).Decode(&sandbox)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return sandbox, nil
}

func (r *SandboxRepository) FreeIP(ctx context.Context, ip string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.allocatedIPs, ip)
}
