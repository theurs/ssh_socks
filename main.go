package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/getlantern/systray"
	"golang.org/x/crypto/ssh"
)

// --- Structures and Global Variables ---

type ServerProfile struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	User      string `json:"user"`
	Host      string `json:"host"`
	SSHPort   string `json:"ssh_port"`
	SocksPort string `json:"socks_port"`
	HttpPort  string `json:"http_port"`
}

type Config struct {
	Profiles         []ServerProfile `json:"profiles"`
	ActiveProfileID  string          `json:"active_profile_id"`
	AutoConnect      bool            `json:"auto_connect"`
	Country          string          `json:"country"`
	HttpProxyEnabled bool            `json:"http_proxy_enabled"`

	// Оставляем старые поля для обратной совместимости при миграции
	User      string `json:"user,omitempty"`
	Host      string `json:"host,omitempty"`
	SSHPort   string `json:"ssh_port,omitempty"`
	SocksPort string `json:"socks_port,omitempty"`
}

var countries = []struct {
	ID   string
	Name string
}{
	{"direct", "🚀 Direct Connection (SSH)"},
	{"au", "🇦🇺 Australia"},
	{"br", "🇧🇷 Brazil"},
	{"ca", "🇨🇦 Canada"},
	{"ch", "🇨🇭 Switzerland"},
	{"de", "🇩🇪 Germany"},
	{"es", "🇪🇸 Spain"},
	{"fi", "🇫🇮 Finland"},
	{"fr", "🇫🇷 France"},
	{"gb", "🇬🇧 United Kingdom"},
	{"hk", "🇭🇰 Hong Kong"},
	{"il", "🇮🇱 Israel"},
	{"in", "🇮🇳 India"},
	{"it", "🇮🇹 Italy"},
	{"jp", "🇯🇵 Japan"},
	{"kr", "🇰🇷 South Korea"},
	{"nl", "🇳🇱 Netherlands"},
	{"no", "🇳🇴 Norway"},
	{"pl", "🇵🇱 Poland"},
	{"se", "🇸🇪 Sweden"},
	{"sg", "🇸🇬 Singapore"},
	{"ua", "🇺🇦 Ukraine"},
	{"us", "🇺🇸 USA (United States)"},
}

var countryMenuItems = make(map[string]*systray.MenuItem)

var (
	conf      Config
	confMutex sync.Mutex
	sshCmd    *exec.Cmd
	stopChan  = make(chan bool, 1)
	passDone  = make(chan bool, 1)

	mConnect, mDisconnect *systray.MenuItem
	mHttpProxy            *systray.MenuItem // HTTP Proxy toggle

	// WinAPI DLLs
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")

	// WinAPI Procedures
	procCreateWindow  = user32.NewProc("CreateWindowExW")
	procDefWinProc    = user32.NewProc("DefWindowProcW")
	procRegisterClass = user32.NewProc("RegisterClassExW")
	procGetMsg        = user32.NewProc("GetMessageW")
	procDispatchMsg   = user32.NewProc("DispatchMessageW")
	procTranslateMsg  = user32.NewProc("TranslateMessage")
	procSendMessage   = user32.NewProc("SendMessageW")
	procShowWindow    = user32.NewProc("ShowWindow")
	procDestroyWin    = user32.NewProc("DestroyWindow")
	procGetWinText    = user32.NewProc("GetWindowTextW")
	procPostQuitMsg   = user32.NewProc("PostQuitMessage")
	procMessageBox    = user32.NewProc("MessageBoxW")
	procSetWindowText = user32.NewProc("SetWindowTextW")

	// Window Handles
	hEdUser, hEdHost, hEdSSH, hEdSocks, hCbAuto uintptr
	hPassEdit                                   uintptr
	hComboProfiles, hBtnAdd, hBtnDel, hEdName   uintptr
	hEdHttp                                     uintptr // HTTP Proxy port field
	tempPassword                                string
	settingsOpen                                bool
	loopRunning                                 bool
	loopMutex                                   sync.Mutex

	editProfiles   []ServerProfile
	editCurrentIdx int

	mServers             *systray.MenuItem
	serverMenuItems      = make(map[string]*systray.MenuItem)
	activeRunningProfile *ServerProfile // Хранит профиль, который сейчас запущен

	// HTTP Proxy
	httpProxyServer *HttpProxy
)

const (
	StateDisconnected = iota
	StateConnected
	StateError

	WM_COMMAND  = 0x0111
	WM_DESTROY  = 0x0002
	WM_SETFONT  = 0x0030
	BM_GETCHECK = 0x00F0
	BM_SETCHECK = 0x00F1
	BST_CHECKED = 0x0001
	ES_PASSWORD = 0x0020

	CBS_DROPDOWNLIST = 0x0003
	CBN_SELCHANGE    = 1
	CB_ADDSTRING     = 0x0143
	CB_SETCURSEL     = 0x014E
	CB_GETCURSEL     = 0x0147
	CB_RESETCONTENT  = 0x014B

	ID_BUTTON_SAVE    = 1
	ID_BUTTON_PASS    = 2
	ID_CHECKBOX       = 3
	ID_COMBO_PROFILES = 4
	ID_BTN_ADD        = 5
	ID_BTN_DEL        = 6
	ID_BTN_HTTP       = 7 // Toggle HTTP proxy
)

func main() {
	loadConfig()
	systray.Run(onReady, onExit)
}

func onReady() {
	systray.SetTitle("SSH Proxy")
	mConnect = systray.AddMenuItem("Connect", "Start proxy")
	mDisconnect = systray.AddMenuItem("Disconnect", "Stop proxy")
	systray.AddSeparator()

	mServers = systray.AddMenuItem("Servers", "Select active server")
	rebuildServersMenu()

	mRoute := systray.AddMenuItem("Traffic Route", "Select exit country")
	for _, c := range countries {
		isChecked := conf.Country == c.ID
		if conf.Country == "" && c.ID == "direct" {
			isChecked = true
		}

		mItem := mRoute.AddSubMenuItemCheckbox(c.Name, "", isChecked)
		countryMenuItems[c.ID] = mItem

		go func(id string) {
			for range mItem.ClickedCh {
				handleCountryChange(id)
			}
		}(c.ID)
	}

	systray.AddSeparator()
	mHttpProxy = systray.AddMenuItemCheckbox("Enable HTTP Proxy", "Run HTTP proxy on configured port", conf.HttpProxyEnabled)
	systray.AddSeparator()
	mSettings := systray.AddMenuItem("Settings", "Connection parameters")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Exit", "Close application")

	registerWindowClass("SSHProxySettings", syscall.NewCallback(settingsWndProc))
	registerWindowClass("SSHPassPrompt", syscall.NewCallback(passWndProc))

	setState(StateDisconnected)

	if conf.AutoConnect && len(conf.Profiles) > 0 {
		go startSSHLoop()
	}

	go func() {
		for {
			select {
			case <-mConnect.ClickedCh:
				go startSSHLoop()
			case <-mDisconnect.ClickedCh:
				stopSSH()
				setState(StateDisconnected)
			case <-mHttpProxy.ClickedCh:
				handleHttpProxyToggle()
			case <-mSettings.ClickedCh:
				if !settingsOpen {
					go showSettingsNative()
				}
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

func onExit() {
	stopSSH()
}

func handleHttpProxyToggle() {
	if mHttpProxy.Checked() {
		mHttpProxy.Uncheck()
		// Stop HTTP proxy if running
		if httpProxyServer != nil {
			httpProxyServer.Stop()
			httpProxyServer = nil
		}
		confMutex.Lock()
		conf.HttpProxyEnabled = false
		confMutex.Unlock()
		saveConfig()
	} else {
		mHttpProxy.Check()
		// Start HTTP proxy if SSH is connected
		if activeRunningProfile != nil && activeRunningProfile.HttpPort != "" {
			socksAddr := net.JoinHostPort("127.0.0.1", activeRunningProfile.SocksPort)
			httpAddr := net.JoinHostPort("127.0.0.1", activeRunningProfile.HttpPort)
			httpProxyServer = NewHttpProxy(httpAddr, socksAddr)
			if err := httpProxyServer.Start(); err != nil {
				showError("HTTP Proxy Error", "Failed to start HTTP proxy:\n"+err.Error())
				mHttpProxy.Uncheck()
			} else {
				confMutex.Lock()
				conf.HttpProxyEnabled = true
				confMutex.Unlock()
				saveConfig()
			}
		}
	}
}

func rebuildServersMenu() {
	// Скрываем старые пункты
	for _, item := range serverMenuItems {
		item.Hide()
	}

	confMutex.Lock()
	profiles := conf.Profiles
	activeID := conf.ActiveProfileID
	confMutex.Unlock()

	if len(profiles) == 0 {
		mServers.Disable()
		return
	}
	mServers.Enable()

	for _, p := range profiles {
		if _, exists := serverMenuItems[p.ID]; !exists {
			item := mServers.AddSubMenuItemCheckbox(p.Name, "", activeID == p.ID)
			serverMenuItems[p.ID] = item
			go func(id string, m *systray.MenuItem) {
				for range m.ClickedCh {
					setActiveServerFromTray(id)
				}
			}(p.ID, item)
		} else {
			serverMenuItems[p.ID].SetTitle(p.Name)
			if activeID == p.ID {
				serverMenuItems[p.ID].Check()
			} else {
				serverMenuItems[p.ID].Uncheck()
			}
			serverMenuItems[p.ID].Show()
		}
	}
}

func setActiveServerFromTray(id string) {
	confMutex.Lock()
	conf.ActiveProfileID = id
	confMutex.Unlock()
	saveConfig()

	rebuildServersMenu()

	loopMutex.Lock()
	active := loopRunning
	loopMutex.Unlock()

	if active {
		stopSSH()
		go startSSHLoop()
	}
}

func handleCountryChange(newID string) {
	confMutex.Lock()
	oldID := conf.Country
	if oldID == "" {
		oldID = "direct"
	}
	conf.Country = newID
	confMutex.Unlock()

	for id, item := range countryMenuItems {
		if id == newID {
			item.Check()
		} else {
			item.Uncheck()
		}
	}

	saveConfig()

	if newID != "direct" {
		systray.SetTooltip("Status: Checking Tor on server...")
		err := checkAndInstallTor()
		if err != nil {
			showError("Server Error", "Tor not found and cannot be installed:\n"+err.Error())
			handleCountryChange("direct")
			return
		}
	}

	loopMutex.Lock()
	active := loopRunning
	loopMutex.Unlock()
	if active {
		stopSSH()
		go startSSHLoop()
	}
}

func checkAndInstallTor() error {
	confMutex.Lock()
	prof := getActiveProfile()
	confMutex.Unlock()

	if prof == nil || prof.Host == "" {
		return fmt.Errorf("please configure a server profile first")
	}
	user, host, port := prof.User, prof.Host, prof.SSHPort

	checkCmd := `if ! command -v tor >/dev/null 2>&1; then
		echo "Tor missing, attempting install...";
		sudo apt-get update -qq && sudo apt-get install -y tor;
	fi && command -v tor`

	args := []string{"-p", port, "-o", "StrictHostKeyChecking=no", fmt.Sprintf("%s@%s", user, host), checkCmd}
	cmd := exec.Command("ssh", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Tor is not installed. Try running 'sudo apt install tor' on the server manually.\nError: %v\nOutput: %s", err, string(output))
	}

	return nil
}

func setupRemoteTor(countryCode string) error {
	confMutex.Lock()
	prof := getActiveProfile()
	confMutex.Unlock()

	if prof == nil || prof.Host == "" {
		return fmt.Errorf("please configure a server profile first")
	}
	user, host, port := prof.User, prof.Host, prof.SSHPort

	remoteSocksPort := "9060"
	script := fmt.Sprintf(`
# Определяем абсолютный путь к дому
H=$HOME
CFG="$H/.ssh_proxy_torrc"
PID="$H/.ssh_proxy_tor.pid"
DATA="$H/.ssh_proxy_data"

# 1. Жестко убиваем старый процесс, если файл PID существует
if [ -f "$PID" ]; then
    kill $(cat "$PID") > /dev/null 2>&1 || true
    rm "$PID"
fi

# 2. На всякий случай убиваем всё, что слушает 9060 (наш порт)
fuser -k %s/tcp > /dev/null 2>&1 || true

# 3. Создаем чистую папку для данных
mkdir -p "$DATA"

# 4. Генерируем конфиг с АБСОЛЮТНЫМИ путями
cat <<EOF > "$CFG"
SocksPort 127.0.0.1:%s
DataDirectory $DATA
PidFile $PID
ExitNodes {%s}
StrictNodes 1
EOF

# 5. Запускаем
nohup tor -f "$CFG" > "$DATA/tor.log" 2>&1 &

# 6. Ждем готовности порта (до 20 секунд)
for i in {1..20}; do
    if ss -nlt | grep -q ":%s"; then
        exit 0
    fi
    sleep 1
done
exit 1
`, remoteSocksPort, remoteSocksPort, countryCode, remoteSocksPort)

	args := []string{"-p", port, "-o", "StrictHostKeyChecking=no", fmt.Sprintf("%s@%s", user, host), "bash"}
	cmd := exec.Command("ssh", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Stdin = strings.NewReader(script)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Tor setup failed: %v\n%s", err, string(output))
	}
	return nil
}

// --- SSH and Keys Logic ---

func ensureSSHKeys() (string, error) {
	home, _ := os.UserHomeDir()
	sshDir := filepath.Join(home, ".ssh")
	privKey := filepath.Join(sshDir, "id_rsa")
	pubKey := privKey + ".pub"

	if _, err := os.Stat(pubKey); os.IsNotExist(err) {
		os.MkdirAll(sshDir, 0700)
		cmd := exec.Command("ssh-keygen", "-t", "rsa", "-b", "2048", "-N", "", "-f", privKey)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if err := cmd.Run(); err != nil {
			return "", err
		}
	}
	content, err := ioutil.ReadFile(pubKey)
	return string(content), err
}

func uploadKeyToServer(pubKey string) error {
	confMutex.Lock()
	prof := getActiveProfile()
	confMutex.Unlock()

	if prof == nil || prof.Host == "" {
		return fmt.Errorf("please configure a server profile first")
	}
	user, host, port := prof.User, prof.Host, prof.SSHPort

	check := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=3", "-o", "StrictHostKeyChecking=no", "-p", port, fmt.Sprintf("%s@%s", user, host), "exit")
	check.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := check.Run(); err == nil {
		return nil
	}

	tempPassword = ""
	go showPasswordPrompt()
	<-passDone

	if tempPassword == "" {
		return fmt.Errorf("authentication cancelled by user")
	}

	sshConfig := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(tempPassword)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	addr := net.JoinHostPort(host, port)
	client, err := ssh.Dial("tcp", addr, sshConfig)
	if err != nil {
		return fmt.Errorf("password or server error: %v", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	setupCmd := fmt.Sprintf("mkdir -p ~/.ssh && chmod 700 ~/.ssh && echo '%s' >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys", pubKey)
	return session.Run(setupCmd)
}

func startSSHLoop() {
	loopMutex.Lock()
	if loopRunning {
		loopMutex.Unlock()
		return
	}
	loopRunning = true
	loopMutex.Unlock()

	defer func() {
		loopMutex.Lock()
		loopRunning = false
		loopMutex.Unlock()
	}()

	select {
	case <-stopChan:
	default:
	}

	// Берем активный профиль и сохраняем его как ТЕКУЩИЙ запущенный
	confMutex.Lock()
	prof := getActiveProfile()
	country := conf.Country
	if prof != nil {
		activeRunningProfile = &ServerProfile{}
		*activeRunningProfile = *prof // копируем
	} else {
		activeRunningProfile = nil
	}
	confMutex.Unlock()

	if activeRunningProfile == nil || activeRunningProfile.Host == "" {
		showError("Profile Error", "No active server configured.")
		setState(StateError)
		return
	}

	pubKey, err := ensureSSHKeys()
	if err != nil {
		showError("Key Error", "Failed to create local SSH key: "+err.Error())
		setState(StateError)
		return
	}

	setState(StateDisconnected)
	systray.SetTooltip(fmt.Sprintf("Auth: %s...", activeRunningProfile.Name))

	if err := uploadKeyToServer(pubKey); err != nil {
		showError("SSH Error", "Failed to authorize on server: "+err.Error())
		setState(StateError)
		return
	}

	for {
		if sshCmd != nil && sshCmd.Process != nil {
			sshCmd.Process.Kill()
		}
		killProcessOnPort(activeRunningProfile.SocksPort)
		time.Sleep(500 * time.Millisecond)

		var args []string

		if country == "direct" || country == "" {
			systray.SetTooltip(fmt.Sprintf("Direct: %s", activeRunningProfile.Name))
			args = []string{"-N", "-T", "-o", "ServerAliveInterval=60", "-o", "StrictHostKeyChecking=no", "-p", activeRunningProfile.SSHPort, "-D", activeRunningProfile.SocksPort, fmt.Sprintf("%s@%s", activeRunningProfile.User, activeRunningProfile.Host)}
		} else {
			systray.SetTooltip(fmt.Sprintf("Starting Tor (%s) on %s...", country, activeRunningProfile.Name))
			if err := setupRemoteTor(country); err != nil {
				showError("Tor Error", err.Error())
				setState(StateError)
				return
			}
			systray.SetTooltip(fmt.Sprintf("Tor (%s): %s", country, activeRunningProfile.Name))
			args = []string{"-N", "-T", "-o", "ServerAliveInterval=60", "-o", "StrictHostKeyChecking=no", "-p", activeRunningProfile.SSHPort, "-L", activeRunningProfile.SocksPort + ":127.0.0.1:9060", fmt.Sprintf("%s@%s", activeRunningProfile.User, activeRunningProfile.Host)}
		}

		sshCmd = exec.Command("ssh", args...)
		sshCmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

		if err := sshCmd.Start(); err != nil {
			setState(StateError)
			return
		}

		setState(StateConnected)

		// Start HTTP proxy if enabled and port is configured
		if conf.HttpProxyEnabled && activeRunningProfile.HttpPort != "" {
			socksAddr := net.JoinHostPort("127.0.0.1", activeRunningProfile.SocksPort)
			httpAddr := net.JoinHostPort("127.0.0.1", activeRunningProfile.HttpPort)
			httpProxyServer = NewHttpProxy(httpAddr, socksAddr)
			if err := httpProxyServer.Start(); err != nil {
				log.Printf("[HTTP Proxy] Warning: Failed to start HTTP proxy: %v", err)
			}
		}

		done := make(chan error, 1)
		go func() { done <- sshCmd.Wait() }()

		select {
		case <-stopChan:
			return
		case <-done:
			setState(StateError)
			select {
			case <-stopChan:
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}
	}
}

func stopSSH() {
	select {
	case stopChan <- true:
	default:
	}

	// Stop HTTP proxy
	if httpProxyServer != nil {
		httpProxyServer.Stop()
		httpProxyServer = nil
	}

	if sshCmd != nil && sshCmd.Process != nil {
		sshCmd.Process.Kill()
	}

	if activeRunningProfile != nil {
		killProcessOnPort(activeRunningProfile.SocksPort)

		confMutex.Lock()
		country := conf.Country
		confMutex.Unlock()

		// Cleanup on server
		if country != "direct" && country != "" && activeRunningProfile.Host != "" {
			go func(u, host, port string) {
				// Use PID file for targeted process termination
				cleanup := `PID="$HOME/.ssh_proxy_tor.pid"; [ -f "$PID" ] && kill $(cat "$PID") && rm "$PID" || true`
				c := exec.Command("ssh", "-p", port, "-o", "StrictHostKeyChecking=no", fmt.Sprintf("%s@%s", u, host), cleanup)
				c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
				c.Run()
			}(activeRunningProfile.User, activeRunningProfile.Host, activeRunningProfile.SSHPort)
		}

		// Сбрасываем активный запущенный профиль
		activeRunningProfile = nil
	}
}

func killProcessOnPort(port string) {
	cmd := exec.Command("cmd", "/c", fmt.Sprintf("netstat -ano | findstr :%s", port))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, _ := cmd.Output()

	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 5 && strings.Contains(fields[1], ":"+port) {
			pid := fields[4]
			if pid != "0" {
				kill := exec.Command("taskkill", "/F", "/PID", pid)
				kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
				kill.Run()
			}
		}
	}
}

// --- WinAPI Windows ---

func settingsWndProc(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	switch msg {
	case WM_COMMAND:
		cmdID := wparam & 0xFFFF
		cmdEvent := (wparam >> 16) & 0xFFFF

		if cmdID == ID_COMBO_PROFILES && cmdEvent == CBN_SELCHANGE {
			saveEditsToCurrentProfile()
			res, _, _ := procSendMessage.Call(hComboProfiles, CB_GETCURSEL, 0, 0)
			editCurrentIdx = int(res)
			loadCurrentProfileToEdits()
			return 0
		}

		if cmdID == ID_BTN_ADD {
			saveEditsToCurrentProfile()
			newProf := ServerProfile{
				ID:        fmt.Sprintf("%d", time.Now().UnixNano()),
				Name:      "New Server",
				User:      "ubuntu",
				SSHPort:   "22",
				SocksPort: "1080",
				HttpPort:  "8080",
			}
			editProfiles = append(editProfiles, newProf)
			editCurrentIdx = len(editProfiles) - 1
			refreshComboBox()
			loadCurrentProfileToEdits()
			return 0
		}

		if cmdID == ID_BTN_DEL {
			if len(editProfiles) > 0 {
				editProfiles = append(editProfiles[:editCurrentIdx], editProfiles[editCurrentIdx+1:]...)
				if editCurrentIdx >= len(editProfiles) {
					editCurrentIdx = len(editProfiles) - 1
				}
				refreshComboBox()
				loadCurrentProfileToEdits()
			}
			return 0
		}

		if cmdID == ID_BUTTON_SAVE {
			saveEditsToCurrentProfile()
			confMutex.Lock()
			conf.Profiles = editProfiles
			if len(editProfiles) > 0 && editCurrentIdx >= 0 {
				conf.ActiveProfileID = editProfiles[editCurrentIdx].ID
			} else {
				conf.ActiveProfileID = ""
			}
			res, _, _ := procSendMessage.Call(hCbAuto, BM_GETCHECK, 0, 0)
			conf.AutoConnect = (res == BST_CHECKED)
			conf.HttpProxyEnabled = mHttpProxy.Checked()
			confMutex.Unlock()
			saveConfig()

			rebuildServersMenu()

			stopSSH()
			if conf.ActiveProfileID != "" {
				go startSSHLoop()
			}
			procDestroyWin.Call(hwnd)
			return 0
		}
	case WM_DESTROY:
		settingsOpen = false
		procPostQuitMsg.Call(0)
	default:
		ret, _, _ := procDefWinProc.Call(hwnd, uintptr(msg), wparam, lparam)
		return ret
	}
	return 0
}

func passWndProc(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	switch msg {
	case WM_COMMAND:
		if wparam == ID_BUTTON_PASS {
			tempPassword = getWinText(hPassEdit)
			passDone <- true
			procDestroyWin.Call(hwnd)
		}
	case WM_DESTROY:
		select {
		case passDone <- true:
		default:
		}
		procPostQuitMsg.Call(0)
	default:
		ret, _, _ := procDefWinProc.Call(hwnd, uintptr(msg), wparam, lparam)
		return ret
	}
	return 0
}

func showSettingsNative() {
	settingsOpen = true
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	confMutex.Lock()
	editProfiles = make([]ServerProfile, len(conf.Profiles))
	copy(editProfiles, conf.Profiles)

	editCurrentIdx = -1
	for i, p := range editProfiles {
		if p.ID == conf.ActiveProfileID {
			editCurrentIdx = i
			break
		}
	}
	if editCurrentIdx == -1 && len(editProfiles) > 0 {
		editCurrentIdx = 0
	}
	autoConn := conf.AutoConnect
	confMutex.Unlock()

	title, _ := syscall.UTF16PtrFromString("SSH Tunnel Settings")
	class, _ := syscall.UTF16PtrFromString("SSHProxySettings")
	// Окно стало чуть выше (520 вместо 480) для HTTP порта
	hwnd, _, _ := procCreateWindow.Call(0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)), 0x00C00000|0x00080000, 200, 200, 360, 520, 0, 0, 0, 0)
	hFont, _, _ := gdi32.NewProc("GetStockObject").Call(17)

	// Блок профилей
	addLabel(hwnd, "Select Profile:", 15, 15, hFont)
	comboC, _ := syscall.UTF16PtrFromString("COMBOBOX")
	hComboProfiles, _, _ = procCreateWindow.Call(0x00000200, uintptr(unsafe.Pointer(comboC)), 0, 0x40000000|0x10000000|CBS_DROPDOWNLIST|0x00200000, 15, 35, 230, 200, hwnd, ID_COMBO_PROFILES, 0, 0)
	procSendMessage.Call(hComboProfiles, WM_SETFONT, hFont, 1)

	btnC, _ := syscall.UTF16PtrFromString("BUTTON")
	tAdd, _ := syscall.UTF16PtrFromString("+")
	tDel, _ := syscall.UTF16PtrFromString("-")
	hBtnAdd, _, _ = procCreateWindow.Call(0, uintptr(unsafe.Pointer(btnC)), uintptr(unsafe.Pointer(tAdd)), 0x40000000|0x10000000, 255, 34, 30, 25, hwnd, ID_BTN_ADD, 0, 0)
	hBtnDel, _, _ = procCreateWindow.Call(0, uintptr(unsafe.Pointer(btnC)), uintptr(unsafe.Pointer(tDel)), 0x40000000|0x10000000, 290, 34, 30, 25, hwnd, ID_BTN_DEL, 0, 0)
	procSendMessage.Call(hBtnAdd, WM_SETFONT, hFont, 1)
	procSendMessage.Call(hBtnDel, WM_SETFONT, hFont, 1)

	// Поля профиля
	addLabel(hwnd, "Profile Name (Alias):", 15, 75, hFont)
	hEdName = addEdit(hwnd, "", 15, 95, 315, hFont, 0)

	addLabel(hwnd, "Remote User:", 15, 130, hFont)
	hEdUser = addEdit(hwnd, "", 15, 150, 315, hFont, 0)

	addLabel(hwnd, "Server Host (IP):", 15, 185, hFont)
	hEdHost = addEdit(hwnd, "", 15, 205, 315, hFont, 0)

	addLabel(hwnd, "SSH Port:", 15, 240, hFont)
	hEdSSH = addEdit(hwnd, "", 15, 260, 140, hFont, 0)

	addLabel(hwnd, "SOCKS Port:", 185, 240, hFont)
	hEdSocks = addEdit(hwnd, "", 185, 260, 145, hFont, 0)

	addLabel(hwnd, "HTTP Port:", 15, 295, hFont)
	hEdHttp = addEdit(hwnd, "", 15, 315, 140, hFont, 0)

	refreshComboBox()
	loadCurrentProfileToEdits()

	// Глобальные настройки
	cbT, _ := syscall.UTF16PtrFromString("Auto-connect on startup")
	hCbAuto, _, _ = procCreateWindow.Call(0, uintptr(unsafe.Pointer(btnC)), uintptr(unsafe.Pointer(cbT)), 0x40000000|0x10000000|0x0003, 15, 355, 315, 25, hwnd, ID_CHECKBOX, 0, 0)
	procSendMessage.Call(hCbAuto, WM_SETFONT, hFont, 1)
	if autoConn {
		procSendMessage.Call(hCbAuto, BM_SETCHECK, BST_CHECKED, 0)
	}

	btnT, _ := syscall.UTF16PtrFromString("Save and Connect")
	hBtn, _, _ := procCreateWindow.Call(0, uintptr(unsafe.Pointer(btnC)), uintptr(unsafe.Pointer(btnT)), 0x40000000|0x10000000|0x0001, 15, 405, 315, 45, hwnd, ID_BUTTON_SAVE, 0, 0)
	procSendMessage.Call(hBtn, WM_SETFONT, hFont, 1)

	procShowWindow.Call(hwnd, 1)
	messageLoop()
}

func showPasswordPrompt() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	title, _ := syscall.UTF16PtrFromString("Server Password Required")
	class, _ := syscall.UTF16PtrFromString("SSHPassPrompt")
	hwnd, _, _ := procCreateWindow.Call(0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)), 0x00C00000|0x00080000, 300, 300, 350, 180, 0, 0, 0, 0)

	hFont, _, _ := gdi32.NewProc("GetStockObject").Call(17)
	addLabel(hwnd, "Enter password to install SSH key:", 15, 15, hFont)
	hPassEdit = addEdit(hwnd, "", 15, 40, 300, hFont, ES_PASSWORD)

	btnT, _ := syscall.UTF16PtrFromString("Confirm")
	btnC, _ := syscall.UTF16PtrFromString("BUTTON")
	hBtn, _, _ := procCreateWindow.Call(0, uintptr(unsafe.Pointer(btnC)), uintptr(unsafe.Pointer(btnT)), 0x40000000|0x10000000, 15, 80, 300, 40, hwnd, ID_BUTTON_PASS, 0, 0)
	procSendMessage.Call(hBtn, WM_SETFONT, hFont, 1)

	procShowWindow.Call(hwnd, 1)
	messageLoop()
}

// --- UI Utilities ---

func showError(title, text string) {
	tPtr, _ := syscall.UTF16PtrFromString(title)
	txtPtr, _ := syscall.UTF16PtrFromString(text)
	procMessageBox.Call(0, uintptr(unsafe.Pointer(txtPtr)), uintptr(unsafe.Pointer(tPtr)), 0x00000010) // MB_ICONERROR
}

func registerWindowClass(name string, proc uintptr) {
	className, _ := syscall.UTF16PtrFromString(name)
	instance, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)
	cursor, _, _ := user32.NewProc("LoadCursorW").Call(0, uintptr(32512))

	type wndClassExW struct {
		cbSize        uint32
		style         uint32
		lpfnWndProc   uintptr
		cbClsExtra    int32
		cbWndExtra    int32
		hInstance     uintptr
		hIcon         uintptr
		hCursor       uintptr
		hbrBackground uintptr
		lpszMenuName  *uint16
		lpszClassName *uint16
		hIconSm       uintptr
	}
	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   proc,
		hInstance:     instance,
		hCursor:       cursor,
		hbrBackground: 16,
		lpszClassName: className,
	}
	procRegisterClass.Call(uintptr(unsafe.Pointer(&wc)))
}

func addLabel(p uintptr, t string, x, y int, f uintptr) {
	tP, _ := syscall.UTF16PtrFromString(t)
	cP, _ := syscall.UTF16PtrFromString("STATIC")
	h, _, _ := procCreateWindow.Call(0, uintptr(unsafe.Pointer(cP)), uintptr(unsafe.Pointer(tP)), 0x40000000|0x10000000, uintptr(x), uintptr(y), 320, 20, p, 0, 0, 0)
	procSendMessage.Call(h, WM_SETFONT, f, 1)
}

func addEdit(p uintptr, t string, x, y, w int, f uintptr, style uint32) uintptr {
	tP, _ := syscall.UTF16PtrFromString(t)
	cP, _ := syscall.UTF16PtrFromString("EDIT")
	h, _, _ := procCreateWindow.Call(0x00000200, uintptr(unsafe.Pointer(cP)), uintptr(unsafe.Pointer(tP)), 0x40000000|0x10000000|0x00800000|uintptr(style), uintptr(x), uintptr(y), uintptr(w), 25, p, 0, 0, 0)
	procSendMessage.Call(h, WM_SETFONT, f, 1)
	return h
}

func getWinText(h uintptr) string {
	buf := make([]uint16, 256)
	procGetWinText.Call(h, uintptr(unsafe.Pointer(&buf[0])), 256)
	return syscall.UTF16ToString(buf)
}

func setWinText(h uintptr, text string) {
	ptr, _ := syscall.UTF16PtrFromString(text)
	procSetWindowText.Call(h, uintptr(unsafe.Pointer(ptr)))
}

func saveEditsToCurrentProfile() {
	if editCurrentIdx >= 0 && editCurrentIdx < len(editProfiles) {
		editProfiles[editCurrentIdx].Name = getWinText(hEdName)
		editProfiles[editCurrentIdx].User = getWinText(hEdUser)
		editProfiles[editCurrentIdx].Host = getWinText(hEdHost)
		editProfiles[editCurrentIdx].SSHPort = getWinText(hEdSSH)
		editProfiles[editCurrentIdx].SocksPort = getWinText(hEdSocks)
		editProfiles[editCurrentIdx].HttpPort = getWinText(hEdHttp)
	}
}

func loadCurrentProfileToEdits() {
	if editCurrentIdx >= 0 && editCurrentIdx < len(editProfiles) {
		p := editProfiles[editCurrentIdx]
		setWinText(hEdName, p.Name)
		setWinText(hEdUser, p.User)
		setWinText(hEdHost, p.Host)
		setWinText(hEdSSH, p.SSHPort)
		setWinText(hEdSocks, p.SocksPort)
		setWinText(hEdHttp, p.HttpPort)
	} else {
		setWinText(hEdName, "")
		setWinText(hEdUser, "")
		setWinText(hEdHost, "")
		setWinText(hEdSSH, "")
		setWinText(hEdSocks, "")
		setWinText(hEdHttp, "8080")
	}
}

func refreshComboBox() {
	procSendMessage.Call(hComboProfiles, CB_RESETCONTENT, 0, 0)
	for _, p := range editProfiles {
		ptr, _ := syscall.UTF16PtrFromString(p.Name)
		procSendMessage.Call(hComboProfiles, CB_ADDSTRING, 0, uintptr(unsafe.Pointer(ptr)))
	}
	if editCurrentIdx >= 0 && editCurrentIdx < len(editProfiles) {
		procSendMessage.Call(hComboProfiles, CB_SETCURSEL, uintptr(editCurrentIdx), 0)
	}
}

func messageLoop() {
	var msg struct {
		Hwnd    uintptr
		Message uint32
		Wparam  uintptr
		Lparam  uintptr
		Time    uint32
		Pt      struct{ X, Y int32 }
	}
	for {
		ret, _, _ := procGetMsg.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if ret == 0 {
			break
		}
		procTranslateMsg.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMsg.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func setState(state int) {
	var c color.RGBA
	var tip string
	switch state {
	case StateDisconnected:
		c = color.RGBA{130, 130, 130, 255}
		tip = "SSH Proxy: Disconnected"
		mConnect.Enable()
		mDisconnect.Disable()
	case StateConnected:
		c = color.RGBA{0, 255, 120, 255}
		tip = "SSH Proxy: Connected"
		mConnect.Disable()
		mDisconnect.Enable()
	case StateError:
		c = color.RGBA{255, 60, 60, 255}
		tip = "SSH Proxy: Error"
		mConnect.Enable()
		mDisconnect.Disable()
	}
	systray.SetIcon(genIcon(c))
	systray.SetTooltip(tip)
}

func genIcon(baseColor color.RGBA) []byte {
	const size = 64
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.Draw(img, image.Rect(6, 10, 58, 54), &image.Uniform{color.RGBA{25, 25, 25, 255}}, image.Point{}, draw.Src)
	for x := 6; x < 58; x++ {
		img.Set(x, 10, baseColor)
		img.Set(x, 53, baseColor)
	}
	for y := 10; y < 54; y++ {
		img.Set(6, y, baseColor)
		img.Set(57, y, baseColor)
	}
	for i := 0; i < 8; i++ {
		img.Set(15+i, 25+i, baseColor)
		img.Set(15+i, 39-i, baseColor)
	}
	for i := 0; i < 12; i++ {
		img.Set(28+i, 40, baseColor)
		img.Set(28+i, 41, baseColor)
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	p := b.Bytes()
	ico := new(bytes.Buffer)
	binary.Write(ico, binary.LittleEndian, []uint16{0, 1, 1})
	ico.Write([]byte{byte(size), byte(size), 0, 0, 1, 0, 32, 0})
	binary.Write(ico, binary.LittleEndian, uint32(len(p)))
	binary.Write(ico, binary.LittleEndian, uint32(22))
	ico.Write(p)
	return ico.Bytes()
}

func loadConfig() {
	dir, _ := os.UserConfigDir()
	path := filepath.Join(dir, "ssh-socks-tray", "config.json")
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &conf)
	} else {
		conf = Config{AutoConnect: false, Country: "direct"}
	}

	// Миграция со старого формата (если есть старый Host, но нет Profiles)
	if len(conf.Profiles) == 0 && conf.Host != "" {
		newProf := ServerProfile{
			ID:        fmt.Sprintf("%d", time.Now().UnixNano()),
			Name:      "Default Server",
			User:      conf.User,
			Host:      conf.Host,
			SSHPort:   conf.SSHPort,
			SocksPort: conf.SocksPort,
			HttpPort:  "8080",
		}
		conf.Profiles = append(conf.Profiles, newProf)
		conf.ActiveProfileID = newProf.ID

		// Очищаем старые поля
		conf.Host, conf.User, conf.SSHPort, conf.SocksPort = "", "", "", ""
		saveConfig()
	}

	// Миграция: добавить HttpPort в существующие профили, если его нет
	for i := range conf.Profiles {
		if conf.Profiles[i].HttpPort == "" {
			conf.Profiles[i].HttpPort = "8080"
		}
	}
}

// Вспомогательная функция для получения текущего профиля
func getActiveProfile() *ServerProfile {
	for i, p := range conf.Profiles {
		if p.ID == conf.ActiveProfileID {
			return &conf.Profiles[i]
		}
	}
	if len(conf.Profiles) > 0 {
		return &conf.Profiles[0]
	}
	return nil
}

func saveConfig() {
	dir, _ := os.UserConfigDir()
	path := filepath.Join(dir, "ssh-socks-tray")
	os.MkdirAll(path, 0755)
	b, _ := json.MarshalIndent(conf, "", "  ")
	os.WriteFile(filepath.Join(path, "config.json"), b, 0644)
}
