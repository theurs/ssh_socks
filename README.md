# SSH Proxy: Hybrid Direct & Tor Tunnel

🇷🇺 [Русская версия (README_RU.md)](./README_RU.md)

**SSH Proxy** is a lightweight, high-performance Windows utility that transforms any remote SSH server into a versatile SOCKS5 gateway. It gives you the best of both worlds: use your server's raw IP for maximum speed, or route through a remote Tor instance for enhanced privacy—all toggleable from your system tray.

![App Screenshot](./pics/2.png)
![App Screenshot](./pics/1.png)
![App Screenshot](./pics/3.png)

## 🌟 Key Features

- 🌍 **Multi-Country Tor Routing** — Choose from 20+ exit countries (US, DE, FR, JP, SG, etc.). Your traffic flows securely: `Local PC -> SSH Tunnel -> Remote Tor Instance -> World`.
- 🚀 **Hybrid Connectivity** — Instantly switch between **Direct Mode** (standard SSH) and **Tor Mode** (anonymized routing) depending on your needs.
- 🛠️ **Zero-Touch Server Setup** — No manual config required. The app automatically detects Tor on your server, installs it if missing (Debian/Ubuntu), and spins up an isolated instance in your user's home directory—leaving system settings untouched.
- 🔐 **Seamless Key Management** — Generates RSA keys locally and deploys them to your server automatically. You only need your password once.
- 🖥️ **Native Windows Build** — Coded in Go using clean WinAPI. It’s tiny, fast, lives in your tray, and supports Windows Autostart.
- 🧹 **Automatic Cleanup** — When you disconnect, the app kills its remote Tor processes to keep your server lean and clutter-free.

## 🚀 How It Works

1.  **Direct Mode**: Uses a standard SSH dynamic port forward (`-D`).
2.  **Tor Mode**: 
    - Spawns an isolated Tor process on your server listening on a local port.
    - Creates an SSH tunnel (`-L`) connecting your PC to that remote Tor port.
    - Traffic exits the Tor network via your chosen `ExitNode`.

## 📥 Installation

1.  Grab the latest binary from [Releases](https://github.com/USERNAME/REPO_NAME/releases).
2.  Extract `ssh_proxy.exe` to a folder of your choice.
3.  Launch the executable.
4.  **Pro Tip**: To keep it running on reboot, copy the file (or a shortcut) into `shell:startup`.
5.  **Browser Tip**: We recommend using an extension like **FoxyProxy** to easily toggle your browser traffic to `127.0.0.1:1080`.

## 🛠️ Getting Started

1.  **Configure**: Right-click the tray icon → **Settings**. Enter your server credentials.
2.  **Pick a Route**: Under the **Traffic Route** menu, select "Direct" or a specific country.
3.  **Connect**: Click **Connect**. The tray icon will turn green once the tunnel is established.
4.  **Proxy Setup**: Set your application's SOCKS5 proxy to `127.0.0.1:1080` (you can customize this port in Settings).

### Configuration Options

| Parameter | Description | Default |
|----------|----------|--------------|
| User | Remote SSH username (regular user recommended) | `ubuntu` |
| Host | Server IP or Domain | — |
| SSH Port | Remote SSH port | `22` |
| SOCKS Port | Local proxy port for your PC | `1080` |
| Autostart | Connect automatically on app launch | `false` |

## 🏗️ Build from Source

### Prerequisites
- Go 1.25+
- OpenSSH client installed on Windows (Standard on Win 10/11).
- A remote server running Debian/Ubuntu (required for automatic Tor installation).

### Compilation
```bash
go build -ldflags "-H=windowsgui -s -w" -o ssh_proxy.exe main.go
```

## ⚠️ Server Requirements
To use Tor Mode effectively:
- **OS**: Debian, Ubuntu, or any distro using `apt`.
- **Permissions**: The user should have `sudo` privileges for the initial Tor install (unless Tor is already installed).
- **Network**: The server must allow internal traffic on port `9060`.

## 📄 License
MIT License — see the [LICENSE](LICENSE) file for details.

---

### Why use this?
Traditional VPNs are easy to detect. This tool uses a standard SSH encrypted tunnel to move your traffic to a remote server before it ever touches the Tor network or the open web, making your proxy usage look like a routine administrative session.