package main

const (
	ConnectorBannerText   = "CPanel Connector"
	ConnectorBannerFont   = "standard"
	ConnectorBannerStrict = true

	ConnectorCopyright = "Copyright (c) 2026 Mihai209"
	ConnectorWebsite   = "https://cpanel-rocky.netlify.app"
	ConnectorSource    = "https://github.com/mihai209/connector-go"
	ConnectorLicense   = "MIT"
)

var (
	// Can be overridden at build time via -ldflags "-X main.ConnectorVersion=1.4.5".
	ConnectorVersion = "1.0.0"
)
