package main

func main() {
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
