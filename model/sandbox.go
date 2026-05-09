package model

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// SecretConfig defines a secret to be injected via the forward proxy.
// The real value never reaches the VM — only the placeholder token is sent as an env var.
// At proxy time, the placeholder is replaced with the real value for allowed hosts only.
type SecretConfig struct {
	Name        string   `bson:"name" json:"name"`               // env var name inside guest: "API_KEY"
	FromEnvVar  string   `bson:"fromEnvVar" json:"from"`         // host env var to read: "OPENAI_API_KEY"
	Hosts       []string `bson:"hosts" json:"hosts"`             // domains where substitution is allowed
	Placeholder string   `bson:"placeholder" json:"-"`           // generated token: "vr_tok_..."
}

// SecretMapping is the runtime representation sent to the proxy (never persisted).
// Contains the actual resolved secret value for substitution.
type SecretMapping struct {
	Placeholder string   `json:"placeholder"`
	Value       string   `json:"value"`
	Hosts       []string `json:"hosts"`
}

// NetworkPolicy defines per-sandbox outbound network rules enforced by the forward proxy.
type NetworkPolicy struct {
	AllowedDomains []string          `bson:"allowed_domains" json:"allowed_domains"`
	BlockedDomains []string          `bson:"blocked_domains" json:"blocked_domains"`
	InjectHeaders  map[string]string `bson:"inject_headers"  json:"inject_headers"`
	SecretMappings []SecretMapping   `bson:"-" json:"secret_mappings,omitempty"`
}

// Sandbox represents the sandbox metadata stored in the database
type Sandbox struct {
	ID               primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Name             string             `bson:"name" json:"name"`
	Image            string             `bson:"image" json:"image"`
	IP               string             `bson:"ip" json:"-"`
	CPU              int                `bson:"cpu" json:"cpu"`
	Mem              int                `bson:"mem" json:"mem"`
	DiskMB           int                `bson:"diskMB" json:"diskMB"`
	Status           string             `bson:"status" json:"status"`
	AutoSleep        bool               `bson:"autoSleep" json:"autoSleep"`
	LastActivityAt   *time.Time         `bson:"lastActivityAt,omitempty" json:"-"`
	PausedAt         *time.Time         `bson:"pausedAt,omitempty" json:"-"`
	StoppedAt        *time.Time         `bson:"stoppedAt,omitempty" json:"-"`
	CreatedAt        time.Time          `bson:"createdAt" json:"createdAt"`
	CreatedBy        primitive.ObjectID `bson:"createdBy" json:"createdBy"`
	OrgID            primitive.ObjectID `bson:"orgId" json:"orgId"`
	EnvVars          map[string]string  `bson:"envVars,omitempty" json:"-"`
	Region           string             `bson:"region,omitempty" json:"region,omitempty"`
	RefID            string             `bson:"refId,omitempty" json:"refId,omitempty"`
	TapName          string             `bson:"tapName,omitempty" json:"-"`
	TapDeleted       bool               `bson:"tapDeleted,omitempty" json:"-"`
	BillingCompleted bool               `bson:"billingCompleted,omitempty" json:"-"`
	NetworkPolicy    *NetworkPolicy     `bson:"network_policy,omitempty" json:"network_policy,omitempty"`
	Secrets          []SecretConfig     `bson:"secrets,omitempty" json:"-"`
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
