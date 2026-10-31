package repository

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
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
	Find(ctx context.Context, filter interface{}, opts options.FindOptions) ([]*model.Sandbox, error)
	SumUsage(ctx context.Context, orgID primitive.ObjectID) (SandboxUsage, error)
	DeleteByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID) (bool, error)
	UpdateStatusByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID, status string) (bool, error)
	MarkDeleted(ctx context.Context, id, orgID primitive.ObjectID) (bool, error)
	UpdateTapNameByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID, tapName string) (bool, error)
	UpdateNetNSByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID, tapName, netnsName string) (bool, error)
	UpdatePublishPortsByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID, ports []int) (bool, error)
	UpdateFieldsByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID, fields bson.M) (bool, error)
	CommitCreate(ctx context.Context, id, orgID primitive.ObjectID, fields bson.M) (bool, error)
	ListCreatingForNode(ctx context.Context, nodeID string) ([]StaleCreate, error)
	DeleteStillCreating(ctx context.Context, ids []primitive.ObjectID) error
	Count(ctx context.Context, orgID primitive.ObjectID, filter interface{}) (int64, error)
	Exists(ctx context.Context, orgID primitive.ObjectID, id string) bool
	FindForHealth(ctx context.Context, nodeID string, opts options.FindOptions) ([]*model.Sandbox, error)
	UpdateStatusForHealth(ctx context.Context, id primitive.ObjectID, status string) error
	UpdateStatusFrom(ctx context.Context, id primitive.ObjectID, from, to string) (bool, error)
	NextAvailableIP() (string, error)
	// Lifecycle management methods
	TouchActivity(ctx context.Context, id primitive.ObjectID) error
	SetRunning(ctx context.Context, id, orgID primitive.ObjectID) (bool, error)
	SetSnapshottedAt(ctx context.Context, id primitive.ObjectID) error
	SetSnapshottedAtAndOrg(ctx context.Context, id, orgID primitive.ObjectID) (bool, error)
	SetPackedAndOrg(ctx context.Context, id, orgID primitive.ObjectID, packPath string) (bool, error)
	SetArchivedAndOrg(ctx context.Context, id, orgID primitive.ObjectID, archiveKey string) (bool, error)
	ClaimColdClear(ctx context.Context, id, orgID primitive.ObjectID) (bool, error)
	ClaimForWake(ctx context.Context, id, orgID primitive.ObjectID) (bool, error)
	ClearPackFieldsAndOrg(ctx context.Context, id, orgID primitive.ObjectID) (bool, error)
	FindSleeping(ctx context.Context, nodeID string) ([]*model.Sandbox, error)
	FindByID(ctx context.Context, id primitive.ObjectID, opts options.FindOneOptions) (*model.Sandbox, error)
	SetBillingCompleted(ctx context.Context, id primitive.ObjectID, completed bool) error
	EnsureBillingIndex(ctx context.Context) error
	ListBillingOpen(ctx context.Context) ([]*model.Sandbox, error)
	ListTerminalTaps(ctx context.Context, limit int64) ([]*model.Sandbox, error)
	MarkTapDeleted(ctx context.Context, id primitive.ObjectID) error
	AggregateHostUsage(ctx context.Context, hostID string) (SandboxHostUsage, error)
	FreeIP(ctx context.Context, ip string)
	ListWithPublishPorts(ctx context.Context, statuses []string) ([]*model.Sandbox, error)
	ListWithPublishPortsForNode(ctx context.Context, nodeID string, statuses []string) ([]*model.Sandbox, error)
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
	r := &SandboxRepository{
		cfg:          cfg,
		collection:   db.Collection("sandboxes"),
		allocatedIPs: make(map[string]bool),
	}
	if cfg != nil {
		r.instancesDir = cfg.Paths.InstancesDir
		r.networkCIDR = cfg.Network.NetworkCIDR
	}
	return r
}

// Init initializes the repository by loading all allocated IPs from the database
func (r *SandboxRepository) Init(ctx context.Context) error {
	// Compound indexes for node-scoped sweeps, org list/count, and legacy cluster-wide lifecycle scans.
	indexes := []mongo.IndexModel{
		{Keys: bson.D{{Key: "orgId", Value: 1}}, Options: options.Index().SetUnique(false)},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "lastActivityAt", Value: 1}}, Options: options.Index().SetUnique(false)},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "snapshottedAt", Value: 1}}, Options: options.Index().SetUnique(false)},
		{
			Keys:    bson.D{{Key: "orgId", Value: 1}, {Key: "labels.$**", Value: 1}},
			Options: options.Index().SetName("orgId_1_labels_wildcard").SetUnique(false),
		},
		{
			Keys:    bson.D{{Key: "orgId", Value: 1}, {Key: "status", Value: 1}, {Key: "_id", Value: -1}},
			Options: options.Index().SetName("orgId_1_status_1__id_-1").SetUnique(false),
		},
		{
			Keys:    bson.D{{Key: "nodeId", Value: 1}, {Key: "status", Value: 1}},
			Options: options.Index().SetName("nodeId_1_status_1").SetUnique(false),
		},
		{
			Keys:    bson.D{{Key: "nodeId", Value: 1}, {Key: "status", Value: 1}, {Key: "lastActivityAt", Value: 1}},
			Options: options.Index().SetName("nodeId_1_status_1_lastActivityAt_1").SetUnique(false),
		},
		{
			Keys:    bson.D{{Key: "nodeId", Value: 1}, {Key: "status", Value: 1}, {Key: "snapshottedAt", Value: 1}},
			Options: options.Index().SetName("nodeId_1_status_1_snapshottedAt_1").SetUnique(false),
		},
	}
	if _, err := r.collection.Indexes().CreateMany(ctx, indexes); err != nil {
		fmt.Printf("[warn] failed to create sandbox indexes: %v\n", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	nodeID := ""
	if r.cfg != nil {
		nodeID = r.cfg.HostID
	}
	cur, err := r.collection.Find(ctx, allocatedIPFilter(nodeID), &options.FindOptions{
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

func (r *SandboxRepository) Find(ctx context.Context, filter interface{}, opts options.FindOptions) ([]*model.Sandbox, error) {
	if filter == nil {
		filter = bson.M{}
	}
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

// SandboxUsage is the total count, CPU, memory, and disk of one org's non-deleted sandboxes.
type SandboxUsage struct {
	Count  int64
	CPU    int64
	MemMB  int64
	DiskMB int64
}

func (r *SandboxRepository) SumUsage(ctx context.Context, orgID primitive.ObjectID) (SandboxUsage, error) {
	cur, err := r.collection.Aggregate(ctx, mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.M{"orgId": orgID, "status": bson.M{"$ne": "deleted"}}}},
		bson.D{{Key: "$group", Value: bson.M{
			"_id":   nil,
			"count": bson.M{"$sum": 1},
			"cpu":   bson.M{"$sum": "$cpu"},
			"mem":   bson.M{"$sum": "$memMB"},
			"disk":  bson.M{"$sum": "$diskMB"},
		}}},
	})
	if err != nil {
		return SandboxUsage{}, err
	}
	defer cur.Close(ctx)
	if !cur.Next(ctx) {
		if err := cur.Err(); err != nil {
			return SandboxUsage{}, err
		}
		return SandboxUsage{}, nil
	}
	var doc struct {
		Count  int64 `bson:"count"`
		CPU    int64 `bson:"cpu"`
		MemMB  int64 `bson:"mem"`
		DiskMB int64 `bson:"disk"`
	}
	if err := cur.Decode(&doc); err != nil {
		return SandboxUsage{}, err
	}
	return SandboxUsage{Count: doc.Count, CPU: doc.CPU, MemMB: doc.MemMB, DiskMB: doc.DiskMB}, nil
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

// UpdateStatusFrom transitions a row from one status to another.
// CAS-guarded on from; returns nil when the row was already in a different state (concurrent transition).
func (r *SandboxRepository) UpdateStatusFrom(ctx context.Context, id primitive.ObjectID, from, to string) (bool, error) {
	res, err := r.collection.UpdateOne(
		ctx,
		bson.M{"_id": id, "status": from},
		bson.M{"$set": bson.M{
			"status":    to,
			"updatedAt": time.Now(),
		}},
	)
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
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

// MarkDeleted moves a row to "deleted" once. A second call matches nothing.
func (r *SandboxRepository) MarkDeleted(ctx context.Context, id, orgID primitive.ObjectID) (bool, error) {
	res, err := r.collection.UpdateOne(ctx, bson.M{
		"_id":    id,
		"orgId":  orgID,
		"status": bson.M{"$ne": "deleted"},
	}, bson.M{"$set": bson.M{
		"status":    "deleted",
		"updatedAt": time.Now(),
	}})
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

// SetRunning marks the sandbox running and stamps lastActivityAt in one write.
func (r *SandboxRepository) SetRunning(ctx context.Context, id, orgID primitive.ObjectID) (bool, error) {
	now := time.Now()
	res, err := r.collection.UpdateOne(ctx, bson.M{"_id": id, "orgId": orgID}, bson.M{"$set": bson.M{
		"status":         "running",
		"updatedAt":      now,
		"lastActivityAt": now,
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

func (r *SandboxRepository) UpdatePublishPortsByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID, ports []int) (bool, error) {
	res, err := r.collection.UpdateOne(ctx, bson.M{"_id": id, "orgId": orgID}, bson.M{"$set": bson.M{
		"publishPorts": ports,
		"updatedAt":    time.Now(),
	}})
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

func (r *SandboxRepository) UpdateFieldsByIDAndOrg(ctx context.Context, id, orgID primitive.ObjectID, fields bson.M) (bool, error) {
	if len(fields) == 0 {
		return false, fmt.Errorf("no fields to update")
	}
	set := bson.M{}
	for k, v := range fields {
		set[k] = v
	}
	set["updatedAt"] = time.Now()
	res, err := r.collection.UpdateOne(ctx, bson.M{"_id": id, "orgId": orgID}, bson.M{"$set": set})
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

// CommitCreate moves a row out of "creating"; false means Delete or a startup sweep took it first.
func (r *SandboxRepository) CommitCreate(ctx context.Context, id, orgID primitive.ObjectID, fields bson.M) (bool, error) {
	set := bson.M{}
	for k, v := range fields {
		set[k] = v
	}
	set["updatedAt"] = time.Now()
	res, err := r.collection.UpdateOne(ctx, bson.M{"_id": id, "orgId": orgID, "status": "creating"}, bson.M{"$set": set})
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

// StaleCreate is a "creating" row left by a crashed process.
type StaleCreate struct {
	ID     primitive.ObjectID
	OrgID  primitive.ObjectID
	CPU    int
	MemMB  int
	DiskMB int
}

// ListCreatingForNode returns rows a crashed process left in "creating" on nodeID.
// Call only before this node serves requests.
func (r *SandboxRepository) ListCreatingForNode(ctx context.Context, nodeID string) ([]StaleCreate, error) {
	if nodeID == "" {
		return nil, nil
	}
	filter := bson.M{"nodeId": nodeID, "status": "creating"}
	cur, err := r.collection.Find(ctx, filter, options.Find().SetProjection(bson.M{
		"_id":    1,
		"orgId":  1,
		"cpu":    1,
		"memMB":  1,
		"diskMB": 1,
	}))
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID     primitive.ObjectID `bson:"_id"`
		OrgID  primitive.ObjectID `bson:"orgId"`
		CPU    int                `bson:"cpu"`
		MemMB  int                `bson:"memMB"`
		DiskMB int                `bson:"diskMB"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]StaleCreate, 0, len(rows))
	for _, row := range rows {
		out = append(out, StaleCreate{
			ID:     row.ID,
			OrgID:  row.OrgID,
			CPU:    row.CPU,
			MemMB:  row.MemMB,
			DiskMB: row.DiskMB,
		})
	}
	return out, nil
}

// DeleteStillCreating removes the given rows only while they are still "creating".
func (r *SandboxRepository) DeleteStillCreating(ctx context.Context, ids []primitive.ObjectID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := r.collection.DeleteMany(ctx, bson.M{"_id": bson.M{"$in": ids}, "status": "creating"})
	return err
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
	_, err := r.collection.UpdateOne(ctx, bson.M{"_id": id}, bson.M{
		"$set": bson.M{
			"status":        "snapshotted",
			"snapshottedAt": now,
			"updatedAt":     now,
		},
		"$unset": bson.M{"coldCleared": ""},
	})
	return err
}

func (r *SandboxRepository) SetSnapshottedAtAndOrg(ctx context.Context, id, orgID primitive.ObjectID) (bool, error) {
	now := time.Now()
	res, err := r.collection.UpdateOne(ctx, bson.M{"_id": id, "orgId": orgID}, bson.M{
		"$set": bson.M{
			"status":        "snapshotted",
			"snapshottedAt": now,
			"updatedAt":     now,
			"packed":        false,
			"packPath":      "",
		},
		"$unset": bson.M{"archiveKey": "", "archivedAt": "", "coldCleared": ""},
	})
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

func (r *SandboxRepository) SetPackedAndOrg(ctx context.Context, id, orgID primitive.ObjectID, packPath string) (bool, error) {
	now := time.Now()
	res, err := r.collection.UpdateOne(ctx, bson.M{"_id": id, "orgId": orgID, "status": "snapshotted"}, bson.M{"$set": bson.M{
		"packed":    true,
		"packPath":  packPath,
		"updatedAt": now,
	}})
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

func (r *SandboxRepository) SetArchivedAndOrg(ctx context.Context, id, orgID primitive.ObjectID, archiveKey string) (bool, error) {
	now := time.Now()
	res, err := r.collection.UpdateOne(ctx, bson.M{
		"_id": id, "orgId": orgID, "status": "snapshotted", "packed": true,
	}, bson.M{"$set": bson.M{
		"status":     "archived",
		"archiveKey": archiveKey,
		"archivedAt": now,
		"packed":     false,
		"packPath":   "",
		"updatedAt":  now,
	}})
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

// ClaimColdClear marks an archived row so the packer may delete local files.
// A wake that already moved the row off archived does not match.
func (r *SandboxRepository) ClaimColdClear(ctx context.Context, id, orgID primitive.ObjectID) (bool, error) {
	res, err := r.collection.UpdateOne(ctx, bson.M{
		"_id": id, "orgId": orgID, "status": "archived", "coldCleared": bson.M{"$ne": true},
	}, bson.M{"$set": bson.M{
		"coldCleared": true,
		"updatedAt":   time.Now(),
	}})
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

// ClaimForWake moves snapshotted or archived to booting before local files are used.
// It does not match a row the packer already marked coldCleared.
func (r *SandboxRepository) ClaimForWake(ctx context.Context, id, orgID primitive.ObjectID) (bool, error) {
	res, err := r.collection.UpdateOne(ctx, bson.M{
		"_id": id, "orgId": orgID,
		"status":      bson.M{"$in": bson.A{"snapshotted", "archived"}},
		"coldCleared": bson.M{"$ne": true},
	}, bson.M{"$set": bson.M{
		"status":    "booting",
		"updatedAt": time.Now(),
	}})
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

func (r *SandboxRepository) ClearPackFieldsAndOrg(ctx context.Context, id, orgID primitive.ObjectID) (bool, error) {
	res, err := r.collection.UpdateOne(ctx, bson.M{"_id": id, "orgId": orgID}, bson.M{
		"$set":   bson.M{"packed": false, "packPath": "", "updatedAt": time.Now()},
		"$unset": bson.M{"archiveKey": "", "archivedAt": ""},
	})
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
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

func (r *SandboxRepository) ListWithPublishPorts(ctx context.Context, statuses []string) ([]*model.Sandbox, error) {
	return r.ListWithPublishPortsForNode(ctx, "", statuses)
}

func (r *SandboxRepository) ListWithPublishPortsForNode(ctx context.Context, nodeID string, statuses []string) ([]*model.Sandbox, error) {
	if len(statuses) == 0 {
		statuses = []string{"running", "snapshotted", "archived", "paused"}
	}
	filter := bson.M{
		"status":       bson.M{"$in": statuses},
		"publishPorts": bson.M{"$exists": true, "$ne": bson.A{}},
	}
	if nodeID = strings.TrimSpace(nodeID); nodeID != "" {
		filter["nodeId"] = nodeID
	}
	opts := &options.FindOptions{
		Projection: bson.M{
			"_id":          1,
			"orgId":        1,
			"ip":           1,
			"status":       1,
			"nodeId":       1,
			"publishPorts": 1,
			"updatedAt":    1,
		},
	}
	cursor, err := r.collection.Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var out []*model.Sandbox
	if err := cursor.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SandboxHostUsage is live sandbox spend on one node.
// CPU and memory count running and booting rows. Disk and count include paused and snapshotted rows.
type SandboxHostUsage struct {
	DiskMB int
	Count  int
	CPU    int
	MemMB  int
}

func (r *SandboxRepository) SetBillingCompleted(ctx context.Context, id primitive.ObjectID, completed bool) error {
	_, err := r.collection.UpdateByID(ctx, id, bson.M{
		"$set": bson.M{"billingCompleted": completed},
	})
	return err
}

func (r *SandboxRepository) EnsureBillingIndex(ctx context.Context) error {
	_, err := r.collection.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{
			{Key: "billingCompleted", Value: 1},
			{Key: "updatedAt", Value: 1},
		},
		Options: options.Index().SetName("billingCompleted_1_updatedAt_1"),
	})
	return err
}

func (r *SandboxRepository) ListBillingOpen(ctx context.Context) ([]*model.Sandbox, error) {
	return r.Find(ctx, bson.M{"billingCompleted": bson.M{"$ne": true}}, options.FindOptions{})
}

func (r *SandboxRepository) ListTerminalTaps(ctx context.Context, limit int64) ([]*model.Sandbox, error) {
	// $nor drops only tapDeleted:true. A false value must still match.
	filter := bson.M{
		"$and": []bson.M{
			{"status": bson.M{"$in": []string{"deleted", "killed"}}},
			{"tapName": bson.M{"$exists": true, "$ne": ""}},
			{"$nor": []bson.M{{"tapDeleted": true}}},
		},
	}
	opts := options.Find().SetProjection(bson.M{"_id": 1, "tapName": 1})
	if limit > 0 {
		opts.SetLimit(limit)
	}
	return r.Find(ctx, filter, *opts)
}

func (r *SandboxRepository) MarkTapDeleted(ctx context.Context, id primitive.ObjectID) error {
	_, err := r.collection.UpdateOne(ctx, bson.M{"_id": id}, bson.M{
		"$set": bson.M{
			"tapDeleted": true,
			"updatedAt":  time.Now(),
		},
	})
	return err
}

func (r *SandboxRepository) AggregateHostUsage(ctx context.Context, hostID string) (SandboxHostUsage, error) {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.M{
			"nodeId": hostID,
			"status": bson.M{"$in": []string{"running", "booting", "snapshotted", "paused"}},
		}}},
		{{Key: "$group", Value: bson.M{
			"_id":   nil,
			"disk":  bson.M{"$sum": "$diskMB"},
			"count": bson.M{"$sum": 1},
			"cpu": bson.M{"$sum": bson.M{"$cond": bson.A{
				bson.M{"$in": bson.A{"$status", bson.A{"running", "booting"}}},
				"$cpu",
				0,
			}}},
			"mem": bson.M{"$sum": bson.M{"$cond": bson.A{
				bson.M{"$in": bson.A{"$status", bson.A{"running", "booting"}}},
				"$memMB",
				0,
			}}},
		}}},
	}
	cur, err := r.collection.Aggregate(ctx, pipeline)
	if err != nil {
		return SandboxHostUsage{}, err
	}
	defer cur.Close(ctx)
	if !cur.Next(ctx) {
		return SandboxHostUsage{}, cur.Err()
	}
	var row struct {
		Disk  int `bson:"disk"`
		Count int `bson:"count"`
		CPU   int `bson:"cpu"`
		Mem   int `bson:"mem"`
	}
	if err := cur.Decode(&row); err != nil {
		return SandboxHostUsage{}, err
	}
	return SandboxHostUsage{DiskMB: row.Disk, Count: row.Count, CPU: row.CPU, MemMB: row.Mem}, cur.Err()
}
