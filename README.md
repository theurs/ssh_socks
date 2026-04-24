# SSH Proxy: Hybrid Direct & Tor Tunnel

🇷🇺 [Русская версия (README_RU.md)](./README_RU.md)

**SSH Proxy** is a lightweight, high-performance Windows utility that transforms any remote SSH server into a versatile SOCKS5 gateway. It gives you the best of both worlds: use your server's raw IP for maximum speed, or route through a remote Tor instance for enhanced privacy—all toggleable from your system tray.

**New: Smart Filtering & Offline Cache!** - Enable "Re-filter-lists" to route only specific blocked domains and IPs through the proxy. Lists are automatically cached locally, so the proxy remains functional even if the source lists are temporarily unavailable.

![App Screenshot](./pics/2.png)
![App Screenshot](./pics/1.png)
![App Screenshot](./pics/3.png)

## 🌟 Key Features

- 🌍 **Multi-Country Tor Routing** — Choose from 21+ exit countries (US, DE, FR, IL, JP, SG, etc.). Your traffic flows securely: `Local PC -> SSH Tunnel -> Remote Tor Instance -> World`.
- 🚀 **Hybrid Connectivity** — Instantly switch between **Direct Mode** (standard SSH) and **Tor Mode** (anonymized routing) depending on your needs.
- 🛡️ **Smart Traffic Filtering** — Optional "Re-filter" mode allows you to proxy only specific sites and IP ranges from community-maintained lists, leaving other traffic direct for maximum speed.
- 💾 **Offline Cache** — Downloaded filter lists are saved to your local disk. If the network is down or GitHub is unreachable during startup, the app will use the last successfuly cached version.
- 🌐 **Built-in HTTP Proxy** — Enable an HTTP proxy alongside SOCKS5 for applications that don't support SOCKS. Fully configurable port.
- 🛠️ **Zero-Touch Server Setup** — No manual config required. The app automatically detects Tor on your server, installs it if missing (Debian/Ubuntu), and spins up an isolated instance in your user's home directory.
- 🔐 **Seamless Key Management** — Generates RSA keys locally and deploys them to your server automatically. You only need your password once.
- 📝 **Diagnostic Logging** — All connections, routing decisions (proxy vs direct), and errors are logged to the AppData folder for easy troubleshooting.
- 🧹 **Automatic Cleanup** — When you disconnect, the app kills its remote Tor processes to keep your server lean and clutter-free.

## 🚀 How It Works

1.  **Direct Mode**: Uses a standard SSH dynamic port forward (`-D`).
2.  **Tor Mode**: 
    - Spawns an isolated Tor process on your server listening on a local port.
    - Creates an SSH tunnel (`-L`) connecting your PC to that remote Tor port.
    - Traffic exits the Tor network via your chosen `ExitNode`.
3.  **Filter Mode**: When enabled, the app acts as a "smart" SOCKS5 dispatcher, checking each request against local domain and IP lists to decide whether to use the tunnel or connect directly.

## 📥 Installation

1.  Grab the latest binary from [Releases](https://github.com/USERNAME/REPO_NAME/releases).
2.  Extract `ssh_proxy.exe` to a folder of your choice.
3.  Launch the executable.
4.  **Pro Tip**: To keep it running on reboot, copy the file (or a shortcut) into `shell:startup`.

## 🛠️ Getting Started

1.  **Configure**: Right-click the tray icon → **Settings**. Enter your server credentials.
2.  **Pick a Route**: Under the **Traffic Route** menu, select "Direct" or a specific country.
3.  **Enable Filtering**: Check **Re-filter-lists** in the tray menu if you want to proxy only specific blocked resources.
4.  **Connect**: Click **Connect**. The tray icon will turn green once the tunnel is established.
5.  **Proxy Setup**: 
    - **SOCKS5**: Set your application's SOCKS5 proxy to `127.0.0.1:1080` (default port).
    - **HTTP**: Set your application's HTTP proxy to `127.0.0.1:8080`.

### Configuration Options

| Parameter | Description | Default |
|----------|----------|--------------|
| User | Remote SSH username | `ubuntu` |
| Host | Server IP or Domain | — |
| SSH Port | Remote SSH port | `22` |
| SOCKS Port | Local SOCKS5 proxy port | `1080` |
| HTTP Port | Local HTTP proxy port | `8080` |
| Bind Address | Local IP to bind the proxy (e.g. 127.0.0.1) | `127.0.0.1` |
| Autostart | Connect automatically on app launch | `false` |

## 📁 Paths and Logs

All configuration, logs, and caches are stored in:
`%APPDATA%\ssh-socks-tray\`

- `logs/debug.log`: General application operations and errors.
- `logs/proxy.log`: History of proxied requests and routing reasons.
- `filters/*.cache`: Locally saved filter lists for offline use.

## 🏗️ Build from Source

### Prerequisites
- Go 1.25+
- OpenSSH client installed on Windows.

### Compilation
```bash
go build -ldflags "-H=windowsgui -s -w" -o ssh_proxy.exe .
```

## ⚠️ Server Requirements
- **OS**: Debian, Ubuntu, or any distro using `apt`.
- **Permissions**: Sudo privileges for the initial Tor install.
- **Network**: The server must allow internal traffic on port `9060`.

## 📄 License
MIT License — see the [LICENSE](LICENSE) file for details.
