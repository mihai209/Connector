# CPanel Connector (Go)

## English

`connector-go` is the runtime daemon used by CPanel for Docker lifecycle, console I/O, files, schedules, metrics, and SFTP.

- Repo: https://github.com/mihai209/Connector
- License: MIT

### Requirements

- Go 1.22+
- Docker engine + Docker CLI
- Linux host
- Optional host tools for archive extraction:
  - `unzip`
  - `tar`
  - `gzip` / `gunzip`
  - `bzip2`
  - `xz`

### Build

```bash
cd /home/mihai/Desktop/cpanel/connector-go
go mod tidy
go build -o cpanel-connector-go ./
```

### Run

```bash
CONNECTOR_CONFIG=/etc/cpanel-connector/config.json \
VOLUMES_PATH=/var/lib/cpanel/volumes \
./cpanel-connector-go
```

### What It Handles

- Connector authentication over WebSocket
- Panic-safe WS handler dispatch (runtime recover + stack log)
- Docker create/start/stop/restart/kill/delete
- Live console stream + command execution (stdin)
- Console command throttling per server (anti-flood)
- File ops:
  - list/read/write
  - create folder
  - rename/delete/chmod
  - download
  - unarchive in background (`extract_archive`)
- Per-server schedule actions
- Runtime metrics + heartbeat
- Runtime health endpoint: `GET /healthz`
- Runtime readiness endpoint: `GET /readyz` (503 when WS to panel is down)
- SFTP service with panel-backed auth

### Minimal `config.json`

```json
{
  "panel": {
    "url": "https://panel.example.com",
    "allowedUrls": ["https://panel.example.com"]
  },
  "api": {
    "allowedOrigins": ["https://panel.example.com"]
  },
  "connector": {
    "id": 1,
    "token": "REPLACE_WITH_CONNECTOR_TOKEN",
    "name": "node-1"
  },
  "sftp": {
    "host": "0.0.0.0",
    "port": 8312,
    "directory": "/var/lib/cpanel/volumes",
    "hostKeyPath": "/etc/cpanel-connector/sftp_host_rsa.key"
  },
  "docker": {
    "network": {
      "name": "cpanel_nw",
      "network_mode": "cpanel_nw",
      "driver": "bridge",
      "interface": "172.18.0.1",
      "dns": ["1.1.1.1", "1.0.0.1"],
      "enable_icc": true,
      "network_mtu": 1500
    }
  }
}
```

### Update Mode

```bash
./cpanel-connector-go --update
```

Checks: `https://github.com/mihai209/Connector/releases`

### systemd Example

`/etc/systemd/system/cpanel-connector-go.service`

```ini
[Unit]
Description=CPanel Connector (Go)
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/etc/cpanel-connector
Environment=CONNECTOR_CONFIG=/etc/cpanel-connector/config.json
Environment=VOLUMES_PATH=/var/lib/cpanel/volumes
ExecStart=/etc/cpanel-connector/cpanel-connector-go
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

### Notes

- Panel controls if SFTP is enabled/disabled.
- SFTP username format is per server (`username.random5`).
- SFTP password is the panel account password.
- Connector fails fast if auth token is invalid.

## Romana

`connector-go` este daemon-ul runtime folosit de CPanel pentru Docker lifecycle, consola, fisiere, schedule-uri, metrici si SFTP.

- Repo: https://github.com/mihai209/Connector
- Licenta: MIT

### Cerinte

- Go 1.22+
- Docker engine + Docker CLI
- Host Linux
- Optional, utilitare host pentru extract arhive:
  - `unzip`
  - `tar`
  - `gzip` / `gunzip`
  - `bzip2`
  - `xz`

### Build

```bash
cd /home/mihai/Desktop/cpanel/connector-go
go mod tidy
go build -o cpanel-connector-go ./
```

### Rulare

```bash
CONNECTOR_CONFIG=/etc/cpanel-connector/config.json \
VOLUMES_PATH=/var/lib/cpanel/volumes \
./cpanel-connector-go
```

### Ce Gestioneaza

- Autentificarea connector-ului pe WebSocket
- Docker create/start/stop/restart/kill/delete
- Stream live de consola + executie comenzi (stdin)
- Operatii fisiere:
  - list/read/write
  - create folder
  - rename/delete/chmod
  - download
  - unarchive in background (`extract_archive`)
- Actiuni schedule per server
- Metrici runtime + heartbeat
- Serviciu SFTP cu autentificare validata de panel

### `config.json` Minim

Foloseste exemplul din sectiunea English de mai sus.

### Mod Update

```bash
./cpanel-connector-go --update
```

Verifica release-urile din: `https://github.com/mihai209/Connector/releases`

### Exemplu systemd

Foloseste serviciul din sectiunea English de mai sus.

### Note

- Panel-ul controleaza daca SFTP este activ sau nu.
- Username-ul SFTP este per server (`username.random5`).
- Parola SFTP este parola contului din panel.
- Connector-ul da fail fast daca token-ul de autentificare devine invalid.
