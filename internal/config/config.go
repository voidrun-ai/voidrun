package config

import (
	"net"
	"os"
	"strconv"
	"strings"
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
}

// Network configuration
type NetworkConfig struct {
	BridgeName   string
	GatewayIP    string
	NetworkCIDR  string
	SubnetPrefix string
	TapPrefix    string
	Nameservers  []string
}

// MongoDB configuration
type MongoConfig struct {
	URI      string
	Database string
}

// System user configuration
type SystemUserConfig struct {
	ID    string
	Name  string
	Email string
}

// Config holds all application configuration
type Config struct {
	Server                ServerConfig
	Paths                 PathsConfig
	Network               NetworkConfig
	Mongo                 MongoConfig
	SystemUser            SystemUserConfig
	Sandbox               SandboxConfig
	Health                HealthConfig
	Metrics               MetricsConfig
	CORS                  CORSConfig
	APIKeyCacheTTLSeconds int
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
}

// Health monitor configuration
type HealthConfig struct {
	Enabled     bool
	IntervalSec int
	Concurrency int
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
	DefaultBridgeName    = "vmbr0"
	DefaultTapPrefix     = "ttap-"
	DefaultGatewayIP     = "192.168.100.1/22"
	DefaultNetworkCIDR   = "192.168.100.0/22"
	// DefaultSubnetPrefix            = "192.168.100."
	DefaultNameservers             = "8.8.8.8,1.1.1.1"
	DefaultMongoURI                = "mongodb://root:Qaz123wsx123@localhost:27017/vr-db?authSource=admin"
	DefaultMongoDB                 = "vr-db"
	DefaultSystemUserName          = "System"
	DefaultSystemUserEmail         = "system@local"
	DefaultSandboxVCPUs            = 1
	DefaultSandboxMemoryMB         = 1024
	DefaultSandboxDiskMB           = 5120 // 5GB
	DefaultSandboxImage            = "debian"
	DefaultSandboxKernelCmdline    = "root=/dev/vda rw init=/sbin/init net.ifnames=0 biosdevname=0"
	DefaultSandboxSyncTimeoutSec   = 10
	DefaultSandboxDebugBootConsole = false
	DefaultOverlayImage            = "overlay.qcow2"
	DefaultSandboxHostname         = "voidrun"
	// Health monitor defaults
	DefaultHealthEnabled          = true
	DefaultHealthIntervalSec      = 60
	DefaultHealthConcurrency      = 16
	DefaultMetricsEnabled         = true
	DefaultMetricsIntervalSec     = 10
	DefaultMetricsDiskIntervalSec = 60
	DefaultMetricsConcurrency     = 16
	DefaultMetricsPath            = "/metrics"
	// CORS defaults
	DefaultCORSEnabled           = true
	DefaultCORSAllowOrigins      = "*"
	DefaultCORSAllowMethods      = "GET,POST,PUT,PATCH,DELETE,OPTIONS"
	DefaultCORSAllowHeaders      = "Authorization,Content-Type,X-API-Key"
	DefaultCORSExposeHeaders     = ""
	DefaultCORSAllowCredentials  = false
	DefaultCORSMaxAgeSec         = 600
	DefaultAPIKeyCacheTTLSeconds = 3600 // 1 hour
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
	return &Config{
		Server: ServerConfig{
			Port: getEnv("SERVER_PORT", DefaultServerPort),
			Host: getEnv("SERVER_HOST", DefaultServerHost),
		},
		Paths: PathsConfig{
			BaseImagesDir: getEnv("BASE_IMAGES_DIR", DefaultBaseImagesDir),
			InstancesDir:  getEnv("INSTANCES_DIR", DefaultInstancesDir),
			KernelPath:    getEnv("KERNEL_PATH", DefaultKernelPath),
			InitrdPath:    getEnv("INITRD_PATH", DefaultInitrdPath),
		},
		Network: NetworkConfig{
			BridgeName:  getEnv("BRIDGE_NAME", DefaultBridgeName),
			GatewayIP:   getEnv("GATEWAY_IP", DefaultGatewayIP),
			NetworkCIDR: getEnv("NETWORK_CIDR", DefaultNetworkCIDR),
			// SubnetPrefix: getEnv("SUBNET_PREFIX", DefaultSubnetPrefix),
			TapPrefix:   getEnv("TAP_PREFIX", DefaultTapPrefix),
			Nameservers: getEnvCSV("DNS_NAMESERVERS", DefaultNameservers),
		},
		Mongo: MongoConfig{
			URI:      getEnv("MONGO_URI", DefaultMongoURI),
			Database: getEnv("MONGO_DB", DefaultMongoDB),
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
		APIKeyCacheTTLSeconds: getEnvInt("API_KEY_CACHE_TTL_SECONDS", DefaultAPIKeyCacheTTLSeconds),
	}
}

// Address returns the server address string
func (c *ServerConfig) Address() string {
	return c.Host + ":" + c.Port
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
