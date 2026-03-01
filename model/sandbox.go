package model

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Sandbox represents the sandbox metadata stored in the database
type Sandbox struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Name      string             `bson:"name" json:"name"`
	ImageId   string             `bson:"imageId" json:"imageId"`
	IP        string             `bson:"ip" json:"ip"`
	CPU       int                `bson:"cpu" json:"cpu"`
	Mem       int                `bson:"mem" json:"mem"`
	DiskMB    int                `bson:"diskMb" json:"diskMb"`
	Status    string             `bson:"status" json:"status"`
	CreatedAt time.Time          `bson:"createdAt" json:"createdAt"`
	CreatedBy primitive.ObjectID `bson:"createdBy" json:"createdBy"`
	OrgID     primitive.ObjectID `bson:"orgId" json:"orgId"`
	EnvVars   map[string]string  `bson:"envVars,omitempty" json:"envVars,omitempty"`
	UserID    primitive.ObjectID `bson:"userId" json:"userId"`
}

type SandboxSpec struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	CPUs       int               `json:"cpus"`
	MemoryMB   int               `json:"memory_mb"`
	DiskMB     int               `json:"disk_mb"`
	IPAddress  string            `json:"ip_address"`
	EnvVars    map[string]string `json:"env_vars"`
	TapName    string            `json:"tap_name"`
	MacAddress string            `json:"mac_address"`
}
