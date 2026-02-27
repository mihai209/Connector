package main

import (
	"fmt"
	"os"
	"os/user"
	"runtime"
	"strings"
	"time"

	figure "github.com/common-nighthawk/go-figure"
)

const (
	ansiReset  = "\x1b[0m"
	ansiBlue   = "\x1b[34m"
	ansiCyan   = "\x1b[36m"
	ansiYellow = "\x1b[33m"
	ansiRed    = "\x1b[31m"
)

func bootTimestamp() string {
	return time.Now().Format("Jan 02 15:04:05.000")
}

func bootInfo(format string, args ...interface{}) {
	fmt.Printf("%sINFO%s: [%s] %s\n", ansiBlue, ansiReset, bootTimestamp(), fmt.Sprintf(format, args...))
}

func bootWarn(format string, args ...interface{}) {
	fmt.Printf("%sWARN%s: [%s] %s\n", ansiYellow, ansiReset, bootTimestamp(), fmt.Sprintf(format, args...))
}

func printBootBanner() {
	fmt.Println()
	banner := figure.NewFigure(ConnectorBannerText, ConnectorBannerFont, ConnectorBannerStrict)
	for _, line := range strings.Split(strings.TrimRight(banner.String(), "\n"), "\n") {
		fmt.Println(ansiCyan + line + ansiReset)
	}
}

func printBootMetadata(configPath string, cfg Config, volumesPath string) {
	now := time.Now()
	zone, offset := now.Zone()
	offsetHours := offset / 3600

	currentUser, userErr := user.Current()
	username := "unknown"
	userID := "unknown"
	groupID := "unknown"
	if userErr == nil {
		username = currentUser.Username
		userID = currentUser.Uid
		groupID = currentUser.Gid
	}

	dockerVersion, dockerErr := runCommand("docker", "--version")
	if dockerErr != nil {
		dockerVersion = "unavailable"
	}

	fmt.Println(ConnectorCopyright)
	fmt.Printf("Website:  %s\n", ConnectorWebsite)
	fmt.Printf("Source:   %s\n", ConnectorSource)
	fmt.Printf("License:  %s\n", ConnectorLicense)
	fmt.Println()

	bootInfo("loading configuration from file config_file=%s", configPath)
	bootInfo("configured connector id=%d name=%s", cfg.Connector.ID, strings.TrimSpace(cfg.Connector.Name))
	bootInfo("configured panel endpoint url=%s", strings.TrimSpace(cfg.Panel.URL))
	bootInfo("configured panel allowlist allowed_origins=%v", cfg.Panel.AllowedURLs)
	bootInfo("configured system timezone timezone=%s utc_offset=%+d", zone, offsetHours)
	bootInfo("configured runtime go_version=%s os=%s arch=%s", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	reportConnectorVersionStatus()
	bootInfo("configured system user success uid=%s gid=%s username=%s", userID, groupID, username)
	bootInfo("configured docker runtime version=%s", dockerVersion)

	enableICC := true
	if cfg.Docker.Network.EnableICC != nil {
		enableICC = *cfg.Docker.Network.EnableICC
	}
	bootInfo(
		"configured docker network name=%s mode=%s driver=%s interface=%s mtu=%d icc=%t ipv6=%t internal=%t attachable=%t dns=%v",
		cfg.Docker.Network.Name,
		cfg.Docker.Network.Mode,
		cfg.Docker.Network.Driver,
		cfg.Docker.Network.Interface,
		cfg.Docker.Network.NetworkMTU,
		enableICC,
		cfg.Docker.Network.EnableIPv6,
		cfg.Docker.Network.IsInternal,
		cfg.Docker.Network.Attachable,
		cfg.Docker.Network.DNS,
	)
	bootInfo("configured docker runtime domainname=%s tmpfs_size_mb=%d pids_limit=%d", cfg.Docker.Domainname, cfg.Docker.TmpfsSize, cfg.Docker.ContainerPidLimit)
	bootInfo("configured data directory volumes_path=%s", volumesPath)
	bootInfo("configured sftp endpoint bind=%s:%d host_key=%s", cfg.SFTP.Host, cfg.SFTP.Port, cfg.SFTP.HostKeyPath)
}

func printStartupBoot(configPath string, cfg Config, volumesPath string) {
	printBootBanner()
	printBootMetadata(configPath, cfg, volumesPath)
	fmt.Println()
}

func bootFatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "%sERROR%s: [%s] %s\n", ansiRed, ansiReset, bootTimestamp(), fmt.Sprintf(format, args...))
}
