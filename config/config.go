package config

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Server configuration
type ServerConfig struct {
	Port string
	Host string
}

// Paths configuration
type PathsConfig struct {
	BaseImagesDir string
	InstancesDir  string
	KernelPath    string
	InitrdPath    string
	CHPath        string // cloud-hypervisor binary (CH_PATH)
}

// Network configuration
type NetworkConfig struct {
	BridgeName  string
	GatewayIP   string
	NetworkCIDR string
	Prefix      string
	Nameservers []string
}

// MongoDB configuration
type MongoConfig struct {
	URI        string
	Database   string
	LogQueries bool
}

// Redis configuration
type RedisConfig struct {
	Mode           string // "single", "cluster", or "sentinel"
	URL            string // for single-node mode
	ClusterAddrs   string // comma-separated list for cluster mode
	SentinelAddrs  string // comma-separated list for sentinel mode
	SentinelMaster string // master name for sentinel mode
	Password       string // optional auth password
	DB             int    // database number (single-node only)
}

// System user configuration
type SystemUserConfig struct {
	ID    primitive.ObjectID
	Name  string
	Email string
	OrgID primitive.ObjectID
}

// AutoLifecycleConfig controls automatic sandbox lifecycle transitions
type AutoLifecycleConfig struct {
	Enabled                   bool
	SnapshotAfterIdleSec      int // auto-snapshot after N seconds of inactivity (default: 60)
	DeleteAfterSnapshottedSec int // auto-delete after N seconds of being snapshotted (default: 604800)
	CheckIntervalSec          int // how often the manager scans (default: 30)
	Concurrency               int // max concurrent snapshot/delete operations (default: 10)
}

// Config holds all application configuration
type Config struct {
	Server                ServerConfig
	Paths                 PathsConfig
	CHBinary              string // absolute cloud-hypervisor binary; set by ResolveDerivedPaths
	Network               NetworkConfig
	Mongo                 MongoConfig
	Redis                 RedisConfig
	Auth                  AuthConfig
	SystemUser            SystemUserConfig
	Sandbox               SandboxConfig
	Health                HealthConfig
	Metrics               MetricsConfig
	CORS                  CORSConfig
	AutoLifecycle         AutoLifecycleConfig
	Monitor               MonitorConfig
	APIKeyCacheTTLSeconds int
	ClerkCacheTTLSeconds  int
	HostID                string
}

// Auth configuration
type AuthConfig struct {
	JWTSecret string
	// Clerk configuration
	ClerkSecretKey      string
	ClerkPublishableKey string
	ClerkJWKSURL        string
	ClerkEnabled        bool
	LocalMode           bool
}

// Sandbox configuration
type SandboxConfig struct {
	DefaultVCPUs        int
	DefaultMemoryMB     int
	DefaultDiskMB       int
	DefaultImage        string
	KernelCmdline       string
	SyncTimeoutSec      int
	DebugBootConsole    bool
	DefaultOverlayImage string
	DefaultHostname     string
	DiskFormat          string
	Seccomp             bool
	BalloonEnabled      bool
	MemoryShared        bool   // sparse snapshot dump when true (shared=on)
	MemoryHugepages     bool   // host hugetlbfs must be configured
	MemoryPrefault      bool   // touch all pages at boot/restore
	MemoryRestoreMode   string // "auto", "copy", or "ondemand"
	DecoupledSnapshot   bool   // metadata-only snapshot + external RAM file
	MemoryBackingMode   string // "legacy", "shared-shm", or "private-tmpfs"
	MemoryAllowSwap     bool   // omit tmpfs noswap so guest RAM can use host swap
}

// Health monitor configuration
type HealthConfig struct {
	Enabled     bool
	IntervalSec int
	Concurrency int
}

// Monitor configuration
type MonitorConfig struct {
	Enabled bool
}

// Metrics configuration
type MetricsConfig struct {
	Enabled         bool
	IntervalSec     int
	DiskIntervalSec int
	Concurrency     int
	Path            string
}

// CORS configuration
type CORSConfig struct {
	Enabled          bool
	AllowOrigins     []string
	AllowMethods     []string
	AllowHeaders     []string
	ExposeHeaders    []string
	AllowCredentials bool
	MaxAgeSec        int
}

// Default configuration values
const (
	DefaultServerPort    = "33944"
	DefaultServerHost    = ""
	DefaultBaseImagesDir = "/var/lib/voidrun/base-images"
	DefaultInstancesDir  = "/var/lib/voidrun/instances"
	DefaultKernelPath    = "/var/lib/voidrun/base-images/vmlinux"
	DefaultInitrdPath    = ""
	DefaultCHPath        = "/usr/local/bin/cloud-hypervisor"
	DefaultBridgeName    = "vmbr0"
	DefaultGatewayIP     = "192.168.100.1/22"
	DefaultNetworkCIDR   = "192.168.100.0/22"
	DefaultNetPrefix     = "vr"
	// DefaultSubnetPrefix            = "192.168.100."
	DefaultNameservers = "8.8.8.8,1.1.1.1"
	DefaultMongoURI    = "mongodb://localhost:27017/vr-db?authSource=admin"
	DefaultMongoDB     = "vr-db"
	DefaultJWTSecret   = "change-me-in-production"
	// Clerk defaults
	DefaultClerkSecretKey           = ""
	DefaultClerkPublishableKey      = ""
	DefaultClerkJWKSURL             = ""
	DefaultClerkEnabled             = false
	DefaultLocalMode                = false
	DefaultSystemUserName           = "System"
	DefaultSystemUserEmail          = "system@local"
	DefaultSandboxVCPUs             = 1
	DefaultSandboxMemoryMB          = 1024
	DefaultSandboxDiskMB            = 10240 // 10GB
	DefaultSandboxImage             = "code"
	DefaultSandboxKernelCmdline     = "root=/dev/vda rw init=/sbin/init net.ifnames=0 biosdevname=0"
	DefaultSandboxSyncTimeoutSec    = 30
	DefaultSandboxDebugBootConsole  = false
	DefaultOverlayImage             = "overlay.qcow2"
	DefaultSandboxHostname          = "voidrun"
	DefaultAuthLocalMode            = false
	DefaultSandboxDiskFormat        = "qcow2"
	DefaultSandboxSeccomp           = true
	DefaultSandboxBalloonEnabled    = true
	DefaultSandboxMemoryShared      = false
	DefaultSandboxMemoryHugepages   = false
	DefaultSandboxMemoryPrefault    = false
	DefaultSandboxMemoryRestoreMode = "auto"
	DefaultSandboxDecoupledSnapshot = false
	DefaultSandboxMemoryBackingMode = "legacy"
	DefaultSandboxMemoryAllowSwap   = false
	MemBackingLegacy                = "legacy"
	MemBackingSharedShm             = "shared-shm"
	MemBackingPrivateTmpfs          = "private-tmpfs"
	// Health monitor defaults
	DefaultHealthEnabled          = true
	DefaultHealthIntervalSec      = 60
	DefaultHealthConcurrency      = 16
	DefaultMetricsEnabled         = true
	DefaultMetricsIntervalSec     = 10
	DefaultMetricsDiskIntervalSec = 10
	DefaultMetricsConcurrency     = 16
	DefaultMetricsPath            = "/metrics"
	// CORS defaults
	DefaultCORSEnabled           = true
	DefaultCORSAllowOrigins      = "*"
	DefaultCORSAllowMethods      = "GET,POST,PUT,PATCH,DELETE,OPTIONS"
	DefaultCORSAllowHeaders      = "Authorization,Content-Type,X-API-Key,X-Org-ID"
	DefaultCORSExposeHeaders     = ""
	DefaultCORSAllowCredentials  = false
	DefaultCORSMaxAgeSec         = 600
	DefaultAPIKeyCacheTTLSeconds = 3600 // 1 hour
	DefaultClerkCacheTTLSeconds  = 1800 // 30 minutes
	// Redis defaults
	DefaultRedisMode           = "single"
	DefaultRedisURL            = ""
	DefaultRedisClusterAddrs   = ""
	DefaultRedisSentinelAddrs  = ""
	DefaultRedisSentinelMaster = ""
	DefaultRedisPassword       = ""
	DefaultRedisDB             = 0
	// Auto-lifecycle defaults
	DefaultAutoLifecycleEnabled                   = true
	DefaultAutoLifecycleSnapshotAfterIdleSec      = 60     // 1 minute
	DefaultAutoLifecycleDeleteAfterSnapshottedSec = 604800 // 1 week
	DefaultAutoLifecycleCheckIntervalSec          = 30     // 30 seconds
	DefaultAutoLifecycleConcurrency               = 10
	// Monitor defaults
	DefaultMonitorEnabled = true
	// Pagination defaults
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// Exec command limits
const (
	MaxCommandLength = 4096
	MaxArgsCount     = 64
	DefaultTimeout   = 30
	MaxTimeout       = 300
	ReadBufferSize   = 16 * 1024
)

// New returns a new Config with default values
func New() *Config {
	c := &Config{
		Server: ServerConfig{
			Port: getEnv("SERVER_PORT", DefaultServerPort),
			Host: getEnv("SERVER_HOST", DefaultServerHost),
		},
		Paths: PathsConfig{
			BaseImagesDir: getEnv("BASE_IMAGES_DIR", DefaultBaseImagesDir),
			InstancesDir:  getEnv("INSTANCES_DIR", DefaultInstancesDir),
			KernelPath:    getEnv("KERNEL_PATH", DefaultKernelPath),
			InitrdPath:    getEnv("INITRD_PATH", DefaultInitrdPath),
			CHPath:        getEnv("CH_PATH", DefaultCHPath),
		},
		Network: NetworkConfig{
			BridgeName:  getEnv("BRIDGE_NAME", DefaultBridgeName),
			GatewayIP:   getEnv("GATEWAY_IP", DefaultGatewayIP),
			NetworkCIDR: getEnv("NETWORK_CIDR", DefaultNetworkCIDR),
			// SubnetPrefix: getEnv("SUBNET_PREFIX", DefaultSubnetPrefix),
			Prefix:      getEnv("NET_PREFIX", DefaultNetPrefix),
			Nameservers: getEnvCSV("DNS_NAMESERVERS", DefaultNameservers),
		},
		Mongo: MongoConfig{
			URI:        getEnv("MONGO_URI", DefaultMongoURI),
			Database:   getEnv("MONGO_DB", DefaultMongoDB),
			LogQueries: getEnvBool("MONGO_LOG_QUERIES", false),
		},
		Redis: RedisConfig{
			Mode:           getEnv("REDIS_MODE", DefaultRedisMode),
			URL:            getEnv("REDIS_URL", DefaultRedisURL),
			ClusterAddrs:   getEnv("REDIS_CLUSTER_ADDRS", DefaultRedisClusterAddrs),
			SentinelAddrs:  getEnv("REDIS_SENTINEL_ADDRS", DefaultRedisSentinelAddrs),
			SentinelMaster: getEnv("REDIS_SENTINEL_MASTER", DefaultRedisSentinelMaster),
			Password:       getEnv("REDIS_PASSWORD", DefaultRedisPassword),
			DB:             getEnvInt("REDIS_DB", DefaultRedisDB),
		},
		Auth: AuthConfig{
			JWTSecret:           getEnv("JWT_SECRET", DefaultJWTSecret),
			ClerkSecretKey:      getEnv("CLERK_SECRET_KEY", DefaultClerkSecretKey),
			ClerkPublishableKey: getEnv("CLERK_PUBLISHABLE_KEY", DefaultClerkPublishableKey),
			ClerkJWKSURL:        getEnv("CLERK_JWKS_URL", DefaultClerkJWKSURL),
			ClerkEnabled:        getEnvBool("CLERK_ENABLED", DefaultClerkEnabled),
			LocalMode:           getEnvBool("AUTH_LOCAL_MODE", DefaultAuthLocalMode),
		},
		SystemUser: SystemUserConfig{
			Name:  getEnv("SYSTEM_USER_NAME", DefaultSystemUserName),
			Email: getEnv("SYSTEM_USER_EMAIL", DefaultSystemUserEmail),
		},
		Sandbox: SandboxConfig{
			DefaultVCPUs:        getEnvInt("SANDBOX_DEFAULT_VCPUS", DefaultSandboxVCPUs),
			DefaultMemoryMB:     getEnvInt("SANDBOX_DEFAULT_MEMORY_MB", DefaultSandboxMemoryMB),
			DefaultDiskMB:       getEnvInt("SANDBOX_DEFAULT_DISK_MB", DefaultSandboxDiskMB),
			DefaultImage:        getEnv("SANDBOX_DEFAULT_IMAGE", DefaultSandboxImage),
			KernelCmdline:       getEnv("SANDBOX_KERNEL_CMDLINE", DefaultSandboxKernelCmdline),
			SyncTimeoutSec:      getEnvInt("SANDBOX_SYNC_TIMEOUT_SEC", DefaultSandboxSyncTimeoutSec),
			DebugBootConsole:    getEnvBool("SANDBOX_DEBUG_BOOT_CONSOLE", DefaultSandboxDebugBootConsole),
			DefaultOverlayImage: getEnv("SANDBOX_DEFAULT_OVERLAY_IMAGE", DefaultOverlayImage),
			DefaultHostname:     getEnv("SANDBOX_DEFAULT_HOSTNAME", DefaultSandboxHostname),
			DiskFormat:          getEnv("SANDBOX_DISK_FORMAT", DefaultSandboxDiskFormat),
			Seccomp:             getEnvBool("SANDBOX_SECCOMP", DefaultSandboxSeccomp),
			BalloonEnabled:      getEnvBool("SANDBOX_BALLOON_ENABLED", DefaultSandboxBalloonEnabled),
			MemoryShared:        getEnvBool("SANDBOX_MEMORY_SHARED", DefaultSandboxMemoryShared),
			MemoryHugepages:     getEnvBool("SANDBOX_MEMORY_HUGEPAGES", DefaultSandboxMemoryHugepages),
			MemoryPrefault:      getEnvBool("SANDBOX_MEMORY_PREFAULT", DefaultSandboxMemoryPrefault),
			MemoryRestoreMode:   getEnv("SANDBOX_MEMORY_RESTORE_MODE", DefaultSandboxMemoryRestoreMode),
			DecoupledSnapshot:   getEnvBool("SANDBOX_DECOUPLED_SNAPSHOT", DefaultSandboxDecoupledSnapshot),
			MemoryBackingMode:   getEnv("SANDBOX_MEMORY_BACKING_MODE", DefaultSandboxMemoryBackingMode),
			MemoryAllowSwap:     getEnvBool("SANDBOX_RAM_ALLOW_SWAP", DefaultSandboxMemoryAllowSwap),
		},
		Health: HealthConfig{
			Enabled:     getEnvBool("HEALTH_ENABLED", DefaultHealthEnabled),
			IntervalSec: getEnvInt("HEALTH_INTERVAL_SEC", DefaultHealthIntervalSec),
			Concurrency: getEnvInt("HEALTH_CONCURRENCY", DefaultHealthConcurrency),
		},
		Metrics: MetricsConfig{
			Enabled:         getEnvBool("METRICS_ENABLED", DefaultMetricsEnabled),
			IntervalSec:     getEnvInt("METRICS_INTERVAL_SEC", DefaultMetricsIntervalSec),
			DiskIntervalSec: getEnvInt("METRICS_DISK_INTERVAL_SEC", DefaultMetricsDiskIntervalSec),
			Concurrency:     getEnvInt("METRICS_CONCURRENCY", DefaultMetricsConcurrency),
			Path:            getEnv("METRICS_PATH", DefaultMetricsPath),
		},
		CORS: CORSConfig{
			Enabled:          getEnvBool("CORS_ENABLED", DefaultCORSEnabled),
			AllowOrigins:     getEnvCSV("CORS_ALLOW_ORIGINS", DefaultCORSAllowOrigins),
			AllowMethods:     getEnvCSV("CORS_ALLOW_METHODS", DefaultCORSAllowMethods),
			AllowHeaders:     getEnvCSV("CORS_ALLOW_HEADERS", DefaultCORSAllowHeaders),
			ExposeHeaders:    getEnvCSV("CORS_EXPOSE_HEADERS", DefaultCORSExposeHeaders),
			AllowCredentials: getEnvBool("CORS_ALLOW_CREDENTIALS", DefaultCORSAllowCredentials),
			MaxAgeSec:        getEnvInt("CORS_MAX_AGE_SEC", DefaultCORSMaxAgeSec),
		},
		AutoLifecycle: AutoLifecycleConfig{
			Enabled:                   getEnvBool("AUTO_LIFECYCLE_ENABLED", DefaultAutoLifecycleEnabled),
			SnapshotAfterIdleSec:      getEnvInt("AUTO_LIFECYCLE_SNAPSHOT_AFTER_IDLE_SEC", DefaultAutoLifecycleSnapshotAfterIdleSec),
			DeleteAfterSnapshottedSec: getEnvInt("AUTO_LIFECYCLE_DELETE_AFTER_SNAPSHOTTED_SEC", DefaultAutoLifecycleDeleteAfterSnapshottedSec),
			CheckIntervalSec:          getEnvInt("AUTO_LIFECYCLE_CHECK_INTERVAL_SEC", DefaultAutoLifecycleCheckIntervalSec),
			Concurrency:               getEnvInt("AUTO_LIFECYCLE_CONCURRENCY", DefaultAutoLifecycleConcurrency),
		},
		Monitor: MonitorConfig{
			Enabled: getEnvBool("MONITOR_ENABLED", DefaultMonitorEnabled),
		},
		APIKeyCacheTTLSeconds: getEnvInt("API_KEY_CACHE_TTL_SECONDS", DefaultAPIKeyCacheTTLSeconds),
		ClerkCacheTTLSeconds:  getEnvInt("CLERK_CACHE_TTL_SECONDS", DefaultClerkCacheTTLSeconds),
		HostID:                hostIDFromEnv(),
	}

	chPath, err := resolveCHBinaryPath(c.Paths.CHPath)
	if err != nil {
		log.Fatalln("Error resolving CH binary path:", err)
	}
	c.CHBinary = chPath

	// Enforce strict length limits on the network prefix to guarantee valid Linux interface names (max 15 chars total)
	if len(c.Network.Prefix) > 4 {
		log.Fatalf("Network.Prefix (NET_PREFIX) must be 4 characters or fewer, got %d chars: %s", len(c.Network.Prefix), c.Network.Prefix)
	}

	// Validate DNS_NAMESERVERS strictly: these values are interpolated verbatim
	// into the per-sandbox iptables-restore ruleset (see runtime/network.go).
	// An invalid or attacker-shaped value (newline, CIDR, blank, etc.) would
	// either break sandbox networking or weaken egress isolation fleet-wide.
	if err := validateNameservers(c.Network.Nameservers); err != nil {
		log.Fatalf("DNS_NAMESERVERS invalid: %v", err)
	}

	if err := validateMemoryBackingMode(c.Sandbox.MemoryBackingMode, c.Sandbox.DecoupledSnapshot); err != nil {
		log.Fatalf("SANDBOX_MEMORY_BACKING_MODE invalid: %v", err)
	}

	return c
}

// validateMemoryBackingMode rejects unknown modes; decoupled snapshot needs file-backed RAM.
func validateMemoryBackingMode(mode string, decoupled bool) error {
	switch mode {
	case MemBackingLegacy, MemBackingSharedShm, MemBackingPrivateTmpfs:
	default:
		return fmt.Errorf("unknown backing mode %q (want one of %q,%q,%q)",
			mode, MemBackingLegacy, MemBackingSharedShm, MemBackingPrivateTmpfs)
	}
	if decoupled && mode == MemBackingLegacy {
		return fmt.Errorf("SANDBOX_DECOUPLED_SNAPSHOT=true requires SANDBOX_MEMORY_BACKING_MODE=%q or %q, got %q",
			MemBackingSharedShm, MemBackingPrivateTmpfs, mode)
	}
	return nil
}

// validateNameservers enforces that each entry is a single, well-formed,
// public unicast IP literal. It rejects CIDRs, blank entries, multicast,
// loopback, link-local, private-range, and unspecified addresses so a
// misconfigured env var cannot silently broaden sandbox egress.
func validateNameservers(nameservers []string) error {
	if len(nameservers) == 0 {
		return fmt.Errorf("at least one nameserver is required")
	}
	for _, ns := range nameservers {
		if ns != strings.TrimSpace(ns) || ns == "" {
			return fmt.Errorf("nameserver %q must be a non-empty, trimmed IP literal", ns)
		}
		ip := net.ParseIP(ns)
		if ip == nil {
			return fmt.Errorf("nameserver %q is not a valid IP literal (CIDRs and hostnames are not allowed)", ns)
		}
		if ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() ||
			ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			ip.IsPrivate() {
			return fmt.Errorf("nameserver %q must be a public unicast address", ns)
		}
	}
	return nil
}

// Address returns the server address string
func (c *ServerConfig) Address() string {
	return c.Host + ":" + c.Port
}

func hostIDFromEnv() string {
	if v := strings.TrimSpace(os.Getenv("NODE_ID")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("HOST_ID")); v != "" {
		return v
	}

	// Fallback to hostname if NODE_ID and HOST_ID are not set
	h, err := os.Hostname()
	if err != nil || strings.TrimSpace(h) == "" {
		return "unknown"
	}
	return strings.TrimSpace(h)
}

func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value, exists := os.LookupEnv(key); exists {
		if intVal, err := strconv.Atoi(value); err == nil {
			return intVal
		}
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value, exists := os.LookupEnv(key); exists {
		switch strings.ToLower(value) {
		case "1", "true", "t", "yes", "y", "on":
			return true
		case "0", "false", "f", "no", "n", "off":
			return false
		}
	}
	return defaultValue
}

func getEnvCSV(key, defaultValue string) []string {
	value := defaultValue
	if env, exists := os.LookupEnv(key); exists {
		value = env
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// GetNetmask converts the NetworkCIDR (e.g., "192.168.100.0/22")
// into a dotted decimal string (e.g., "255.255.252.0").
func (n *NetworkConfig) GetNetmask() string {
	_, ipNet, err := net.ParseCIDR(n.NetworkCIDR)
	if err != nil {
		return "255.255.252.0" // Fallback safety
	}

	mask := ipNet.Mask
	if len(mask) == 4 {
		return net.IPv4(mask[0], mask[1], mask[2], mask[3]).String()
	}
	return "255.255.252.0"
}

// GetCleanGateway strips the CIDR suffix from the gateway IP
// (e.g., "192.168.100.1/22" -> "192.168.100.1").
func (n *NetworkConfig) GetCleanGateway() string {
	// If it contains a slash, parse it as CIDR
	if strings.Contains(n.GatewayIP, "/") {
		ip, _, err := net.ParseCIDR(n.GatewayIP)
		if err == nil {
			return ip.String()
		}
	}
	// Return as is if no slash or error
	return n.GatewayIP
}

func resolveCHBinaryPath(chPath string) (string, error) {
	p := strings.TrimSpace(chPath)
	if p == "" {
		return "", fmt.Errorf("CH binary path is empty (set CH_PATH)")
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("CH binary path: %w", err)
	}
	return filepath.Clean(abs), nil
}
