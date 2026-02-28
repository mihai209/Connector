# CPanel Connector (Go)

Connector-ul nou in Go, cu logica mutata din varianta veche JS si impartita in module, similar ca organizare cu Wings.

## Structura

- `main.go`: bootstrap
- `connectdata.go`: date plain pentru banner + metadata (copyright/source/license)
- `config.go`: incarcare/validare config
- `service.go`: service lifecycle + send helpers
- `ws.go`: websocket loop + dispatcher mesaje panel
- `heartbeat.go`: heartbeat (cpu/ram/disk)
- `events.go`: Docker events monitor
- `logs.go`: console stream + stats stream + buffering
- `install.go`: install/reinstall container flow
- `config_parser.go`: parser pentru `config.files` (json/yaml/properties/ini/text)
- `docker_handlers.go`: power/status/eula/delete handlers
- `schedule_actions.go`: compat layer pentru dispatch schedule actions (`command`/`power`)
- `files.go`: read/write/list/create/rename/delete/permissions
- `sftp.go`: server SFTP + auth in panel + virtual filesystem
- `types.go`: tipuri comune
- `util.go`: utilitare
- `panel_client.go`: HTTP calls catre panel

## Cerinte

- Go 1.22+
- Docker CLI functional
- Linux (recomandat)

## Go packages

- `github.com/gorilla/websocket`
- `github.com/gliderlabs/ssh`
- `github.com/pkg/sftp`
- `github.com/shirou/gopsutil/v3`
- `golang.org/x/crypto`
- `gopkg.in/yaml.v3`
- `github.com/common-nighthawk/go-figure` (figlet-like banner)

## Config

Foloseste `config.json` ca la connector-ul JS:

```json
{
  "panel": {
    "url": "https://panel.example.com",
    "allowedUrls": [
      "https://panel.example.com"
    ]
  },
  "api": {
    "allowedOrigins": [
      "https://panel.example.com"
    ]
  },
  "connector": {
    "id": 1,
    "token": "YOUR_CONNECTOR_TOKEN",
    "name": "node-1"
  },
  "sftp": {
    "host": "0.0.0.0",
    "port": 8312,
    "directory": "/var/lib/cpanel/volumes",
    "hostKeyPath": "/etc/cpanel-connector-go/sftp_host_rsa.key"
  },
  "docker": {
    "domainname": "",
    "tmpfs_size": 100,
    "container_pid_limit": 512,
    "network": {
      "interface": "172.18.0.1",
      "dns": ["1.1.1.1", "1.0.0.1"],
      "name": "cpanel_nw",
      "network_mode": "cpanel_nw",
      "driver": "bridge",
      "is_internal": false,
      "enable_icc": true,
      "network_mtu": 1500,
      "interfaces": {
        "v4": { "subnet": "172.18.0.0/16", "gateway": "172.18.0.1" },
        "v6": { "subnet": "fdba:17c8:6c94::/64", "gateway": "fdba:17c8:6c94::1011" }
      },
      "enableIPv6": false
    }
  }
}
```

`panel.allowedUrls` / `api.allowedOrigins` functioneaza ca allowlist: connector-ul va porni doar daca `panel.url` este in lista.

## Instalare si build

```bash
cd /etc/cpanel-connector-go
cp -r /home/mihai/Desktop/cpanel/connector-go/* .
go mod tidy
go build -o cpanel-connector-go ./
```

## Rulare

```bash
CONNECTOR_CONFIG=/etc/cpanel-connector/config.json \
VOLUMES_PATH=/var/lib/cpanel/volumes \
./cpanel-connector-go
```

## Update rapid (interactive)

```bash
./cpanel-connector-go --update
```

Comanda verifica ultimul release din `https://github.com/mihai209/Connector/releases`, detecteaza asset-ul potrivit pentru OS/ARCH si intreaba daca vrei instalarea.
Updater-ul salveaza si un marker local `<binar>.release.json` pentru a evita redownload pe acelasi release.

## Service systemd (exemplu)

`/etc/systemd/system/cpanel-connector-go.service`

```ini
[Unit]
Description=CPanel Connector (Go)
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/etc/cpanel-connector-go
Environment=CONNECTOR_CONFIG=/etc/cpanel-connector/config.json
Environment=VOLUMES_PATH=/var/lib/cpanel/volumes
ExecStart=/etc/cpanel-connector-go/cpanel-connector-go
Restart=always
RestartSec=3
User=root

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now cpanel-connector-go
sudo systemctl status cpanel-connector-go
```

## Note

- SFTP foloseste username-ul generat in panel (`username.random5`) + parola contului din panel.
- Endpoint panel necesar pentru SFTP auth: `POST /api/connector/sftp-auth`.
- Banner-ul de boot + metadata sunt controlate din `connectdata.go` (usor de modificat pentru modders).
- Connector-ul asigura automat Docker network-ul configurat in `docker.network` (mode/dns/mtu/icc/interfaces) si lanseaza containerele in acel network, in stil Wings.
- Dupa fiecare schimbare mare, ruleaza:

```bash
go mod tidy
go build -o cpanel-connector-go ./
```

## Compatibilitate protocol panel

- Mesajele standard raman: `server_command`, `server_power`, `list_files`, `read_file`, `write_file`, `download_file` etc.
- Pentru compatibilitate forward, connector-ul accepta si alias-uri:
  - `schedule_action` / `server_schedule_action`
  - `file_action` / `server_file_action` (cu `action` in payload)
- Erorile pe fluxurile file/log cleanup includ acum `serverId`, ca panel-ul sa poata corela raspunsul pe serverul corect.
