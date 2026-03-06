package main

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	logBufferMaxLines                   = 500
	logBufferMaxBytes                   = 1024 * 1024
	defaultDiskUsageCacheTTLSeconds     = 10
	notRunningNoticeCooldown            = 5 * time.Second
	heartbeatInterval                   = 10 * time.Second
	wsReconnectDelay                    = 5 * time.Second
	wsReconnectMaxDelay                 = 60 * time.Second
	wsReconnectResetAfter               = 45 * time.Second
	wsWriteTimeout                      = 10 * time.Second
	logAttachRetryDelay                 = 1 * time.Second
	sftpAuthTimeout                     = 7 * time.Second
	defaultVolumesPath                  = "/var/lib/cpanel/volumes"
	defaultSFTPBindHost                 = "0.0.0.0"
	defaultSFTPPort                     = 8312
	defaultAPIPort                      = 2009
	defaultSFTPHostKeyPath              = "./sftp_host_rsa.key"
	defaultDockerNetworkName            = "cpanel_nw"
	defaultDockerNetworkDriver          = "bridge"
	defaultDockerNetworkInterface       = "172.18.0.1"
	defaultDockerNetworkV4Subnet        = "172.18.0.0/16"
	defaultDockerNetworkV4Gateway       = "172.18.0.1"
	defaultDockerNetworkV6Subnet        = "fdba:17c8:6c94::/64"
	defaultDockerNetworkV6Gateway       = "fdba:17c8:6c94::1011"
	defaultDockerNetworkMTU             = int64(1500)
	defaultContainerTmpfsSizeMB         = uint(100)
	defaultContainerPidLimit            = int64(512)
	defaultConsoleThrottleLines         = uint64(2000)
	defaultConsoleThrottleIntervalMs    = uint64(100)
	defaultWSReadLimitMB                = int64(128)
	panelSFTPAuthPath                   = "/api/connector/sftp-auth"
	serverStatsInterval                 = 2 * time.Second
	commandRateWindow                   = 5 * time.Second
	commandRateLimit                    = 40
	maxEditableFileBytes          int64 = 5 * 1024 * 1024
	maxRemoteDownloadBytes        int64 = 512 * 1024 * 1024
	remoteDownloadTimeout               = 8 * time.Minute
)

var defaultDockerDNS = []string{"1.1.1.1", "1.0.0.1"}

type Config struct {
	Panel struct {
		URL            string   `json:"url"`
		AllowedURLs    []string `json:"allowedUrls"`
		AllowedOrigins []string `json:"allowedOrigins"`
	} `json:"panel"`
	API struct {
		Host                 string   `json:"host"`
		Port                 int      `json:"port"`
		AllowedOrigins       []string `json:"allowedOrigins"`
		AllowedOriginsLegacy []string `json:"allowed_origins"`
		TrustedProxies       []string `json:"trustedProxies"`
		SSL                  struct {
			Enabled  bool   `json:"enabled"`
			CertFile string `json:"cert"`
			KeyFile  string `json:"key"`
		} `json:"ssl"`
	} `json:"api"`
	Connector struct {
		ID    int    `json:"id"`
		Token string `json:"token"`
		Name  string `json:"name"`
	} `json:"connector"`
	SFTP struct {
		Host        string `json:"host"`
		Port        int    `json:"port"`
		Directory   string `json:"directory"`
		HostKeyPath string `json:"hostKeyPath"`
		ReadOnly    bool   `json:"readOnly"`
	} `json:"sftp"`
	Docker struct {
		Domainname        string `json:"domainname"`
		TmpfsSize         uint   `json:"tmpfs_size"`
		ContainerPidLimit int64  `json:"container_pid_limit"`
		Rootless          struct {
			Enabled      bool `json:"enabled"`
			ContainerUID int  `json:"container_uid"`
			ContainerGID int  `json:"container_gid"`
		} `json:"rootless"`
		Network           struct {
			Interface string   `json:"interface"`
			DNS       []string `json:"dns"`
			Name      string   `json:"name"`
			ISPN      bool     `json:"ispn"`
			Driver    string   `json:"driver"`
			Mode      string   `json:"network_mode"`

			IsInternal bool  `json:"is_internal"`
			EnableICC  *bool `json:"enable_icc"`
			NetworkMTU int64 `json:"network_mtu"`

			Interfaces struct {
				V4 struct {
					Subnet  string `json:"subnet"`
					Gateway string `json:"gateway"`
				} `json:"v4"`
				V6 struct {
					Subnet  string `json:"subnet"`
					Gateway string `json:"gateway"`
				} `json:"v6"`
			} `json:"interfaces"`

			// Backward-compat with older connector-go config.
			EnableIPv6 bool `json:"enableIPv6"`
			Internal   bool `json:"internal"`
			Attachable bool `json:"attachable"`
		} `json:"network"`
	} `json:"docker"`
	System struct {
		DiskCheckTtlSeconds int   `json:"diskCheckTtlSeconds"`
		WSReadLimitMB       int64 `json:"wsReadLimitMb"`
	} `json:"system"`
	Transfers struct {
		DownloadLimit int `json:"downloadLimit"`
	} `json:"transfers"`
	Throttles struct {
		Enabled           *bool  `json:"enabled"`
		Lines             uint64 `json:"lines"`
		LineResetInterval uint64 `json:"lineResetInterval"`
	} `json:"throttles"`
}

type Service struct {
	cfg         Config
	volumesPath string
	crashPath   string
	wsReadLimitMB int64
	diskUsageCacheTTL       time.Duration
	consoleThrottleEnabled  bool
	consoleThrottleLines    uint64
	consoleThrottleInterval time.Duration
	consoleThrottleMu       sync.Mutex
	consoleThrottle         map[int]ConsoleThrottleState
	downloadLimitBytesPerSec int64

	wsConn    *websocket.Conn
	wsConnMu  sync.RWMutex
	wsWriteMu sync.Mutex

	heartbeatCancel context.CancelFunc

	streamsMu     sync.Mutex
	activeLog     map[int]context.CancelFunc
	activeStat    map[int]context.CancelFunc
	pendingAttach map[int]bool

	buffersMu  sync.Mutex
	logBuffers map[int]*LogBuffer

	cacheMu              sync.Mutex
	diskUsageCache       map[int]DiskUsageCacheEntry
	lastNotRunningNotice map[int]time.Time

	sftpAuthSessions sync.Map // map[string]*SFTPAuthResponse

	attachMu    sync.Mutex
	attachStdin map[int]*AttachedStream

	commandRateMu sync.Mutex
	commandRate   map[int]CommandRateState

	metricsMu sync.Mutex
	metrics   ConnectorMetrics
}

type LogBuffer struct {
	Lines []string
	Bytes int
}

type DiskUsageCacheEntry struct {
	UsedMB int
	TS     time.Time
}

type ServerInstallMessage struct {
	Type      string              `json:"type"`
	ServerID  int                 `json:"serverId"`
	Reinstall bool                `json:"reinstall"`
	Config    ServerInstallConfig `json:"config"`
}

type ServerInstallConfig struct {
	Image          string                 `json:"image"`
	Memory         int                    `json:"memory"`
	CPU            int                    `json:"cpu"`
	Disk           int                    `json:"disk"`
	SwapLimit      int                    `json:"swapLimit"`
	IOWeight       int                    `json:"ioWeight"`
	PidsLimit      int                    `json:"pidsLimit"`
	OOMKillDisable bool                   `json:"oomKillDisable"`
	OOMScoreAdj    int                    `json:"oomScoreAdj"`
	Env            map[string]interface{} `json:"env"`
	Startup        string                 `json:"startup"`
	StartupMode    string                 `json:"startupMode"`
	EggConfig      map[string]interface{} `json:"eggConfig"`
	EggScripts     map[string]interface{} `json:"eggScripts"`
	Installation   map[string]interface{} `json:"installation"`
	ConfigFiles    interface{}            `json:"configFiles"`
	BrandName      string                 `json:"brandName"`
	Ports          []PortMapping          `json:"ports"`
	Mounts         []MountMapping         `json:"mounts"`
}

type PortMapping struct {
	Container int    `json:"container"`
	Host      int    `json:"host"`
	IP        string `json:"ip"`
	Protocol  string `json:"protocol"`
}

type MountMapping struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly"`
}

type SFTPAuthRequest struct {
	ConnectorID int    `json:"connectorId"`
	Token       string `json:"token"`
	Username    string `json:"username"`
	Password    string `json:"password"`
}

type SFTPAuthResponse struct {
	Success bool `json:"success"`
	User    struct {
		ID       int    `json:"id"`
		Username string `json:"username"`
		IsAdmin  bool   `json:"isAdmin"`
	} `json:"user"`
	Servers []struct {
		ID          int    `json:"id"`
		ContainerID string `json:"containerId"`
		Name        string `json:"name"`
	} `json:"servers"`
	Error string `json:"error"`
}

type FileListEntry struct {
	Name        string    `json:"name"`
	IsDirectory bool      `json:"isDirectory"`
	Permissions string    `json:"permissions"`
	Size        int64     `json:"size"`
	MTime       time.Time `json:"mtime"`
}

type AttachedStream struct {
	WriteMu sync.Mutex
	Stdin   io.WriteCloser
}

type CommandRateState struct {
	WindowStart time.Time
	Count       int
}

type ConsoleThrottleState struct {
	WindowStart time.Time
	Count       uint64
	Warned      bool
}

type ConnectorMetrics struct {
	StartTime                  time.Time
	WSConnected                bool
	WSReconnects               int64
	CommandReceivedTotal       int64
	CommandExecutedTotal       int64
	CommandFailedTotal         int64
	PowerReceivedTotal         int64
	PowerExecutedTotal         int64
	PowerFailedTotal           int64
	ScheduleReceivedTotal      int64
	ScheduleExecutedTotal      int64
	ScheduleFailedTotal        int64
	ResourceApplyReceivedTotal int64
	ResourceApplyExecutedTotal int64
	ResourceApplyFailedTotal   int64
	CrashBundlesWrittenTotal   int64
}
