package main

import (
	"os"
	"strings"
)

func main() {
	if len(os.Args) > 1 {
		arg := strings.TrimSpace(strings.ToLower(os.Args[1]))
		if arg == "--update" || arg == "update" {
			if err := runInteractiveSelfUpdate(); err != nil {
				bootFatal("update failed: %v", err)
			}
			return
		}
		if arg == "--version" || arg == "-v" || arg == "version" {
			printConnectorVersionDetails()
			return
		}
	}

	configPath := envOrDefault("CONNECTOR_CONFIG", "./config.json")
	cfg, err := loadConfig(configPath)
	if err != nil {
		bootFatal("failed to load config: %v", err)
		return
	}

	volumesPath := envOrDefault("VOLUMES_PATH", cfg.SFTP.Directory)
	printStartupBoot(configPath, cfg, volumesPath)

	svc := NewService(cfg, volumesPath)

	if err := svc.Start(); err != nil {
		bootFatal("connector crashed: %v", err)
		return
	}
}
