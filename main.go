package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unsafe"

	"github.com/getlantern/systray"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/proxy"
)

// --- Structures and Global Variables ---

type ServerProfile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	User        string `json:"user"`
	Host        string `json:"host"`
	SSHPort     string `json:"ssh_port"`
	SocksPort   string `json:"socks_port"`
	HTTPPort    string `json:"http_port"`
	BindAddress string `json:"bind_address"`
}

type Config struct {
	Profiles         []ServerProfile `json:"profiles"`
	ActiveProfileID  string          `json:"active_profile_id"`
	AutoConnect      bool            `json:"auto_connect"`
	Country          string          `json:"country"`
	HTTPProxyEnabled bool            `json:"http_proxy_enabled"`
	ReFilterEnabled  bool            `json:"re_filter_enabled"`
	User             string          `json:"user,omitempty"`
	Host             string          `json:"host,omitempty"`
	SSHPort          string          `json:"ssh_port,omitempty"`
	SocksPort        string          `json:"socks_port,omitempty"`
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

	filterDomains = make(map[string]struct{})
	filterIPs     = newIPTrie()
	filterMutex   sync.RWMutex
	isListsLoaded bool

	sshCmd   *exec.Cmd
	stopChan = make(chan bool, 1)
	passDone = make(chan bool, 1)

	mConnect, mDisconnect *systray.MenuItem
	mHTTPProxy            *systray.MenuItem
	mReFilter             *systray.MenuItem

	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")

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

	hEdUser, hEdHost, hEdSSH, hEdSocks, hCbAuto uintptr
	hPassEdit                                   uintptr
	hComboProfiles, hBtnAdd, hBtnDel, hEdName   uintptr
	hEdHTTP                                     uintptr
	hEdBind                                     uintptr
	tempPassword                                string
	settingsOpen                                bool
	loopRunning                                 bool
	loopMutex                                   sync.Mutex

	editProfiles   []ServerProfile
	editCurrentIdx int

	mServers             *systray.MenuItem
	serverMenuItems      = make(map[string]*systray.MenuItem)
	activeRunningProfile *ServerProfile

	httpProxyServer *HTTPProxy

	singletonHandle syscall.Handle

	smartSocksListener net.Listener

	logMutex sync.Mutex
)

const (
	StateDisconnected = iota
	StateConnected
	StateError

	WmCommand  = 0x0111
	WmDestroy  = 0x0002
	WmSetFont  = 0x0030
	BmGetCheck = 0x00F0
	BmSetCheck = 0x00F1
	BstChecked = 0x0001
	EsPassword = 0x0020

	CbsDropdownList = 0x0003
	CbnSelChange    = 1
	CbAddString     = 0x0143
	CbSetCursel     = 0x014E
	CbGetCursel     = 0x0147
	CbResetContent  = 0x014B

	IDButtonSave    = 1
	IDButtonPass    = 2
	IDCheckbox      = 3
	IDComboProfiles = 4
	IDBtnAdd        = 5
	IDBtnDel        = 6
	IDBtnHTTP       = 7
)

type trieNode struct {
	children [2]*trieNode
	isEnd    bool
}

type ipTrie struct {
	v4 *trieNode
	v6 *trieNode
}

func newIPTrie() *ipTrie {
	return &ipTrie{v4: &trieNode{}, v6: &trieNode{}}
}

func (t *ipTrie) insert(network *net.IPNet) {
	ip := network.IP
	ones, _ := network.Mask.Size()
	node := t.v4
	if ip.To4() == nil {
		node = t.v6
	} else {
		ip = ip.To4()
	}

	for i := 0; i < ones; i++ {
		bit := (ip[i/8] >> (7 - (i % 8))) & 1
		if node.children[bit] == nil {
			node.children[bit] = &trieNode{}
		}
		node = node.children[bit]
		if node.isEnd {
			return // Уже перекрыто более коротким префиксом
		}
	}
	node.isEnd = true
}

func (t *ipTrie) contains(ip net.IP) bool {
	node := t.v4
	if ip.To4() == nil {
		node = t.v6
	} else {
		ip = ip.To4()
	}
	if node == nil {
		return false
	}

	for i := 0; i < len(ip)*8; i++ {
		bit := (ip[i/8] >> (7 - (i % 8))) & 1
		if node.children[bit] == nil {
			return false
		}
		node = node.children[bit]
		if node.isEnd {
			return true
		}
	}
	return node.isEnd
}

func main() {
	defer handlePanic()
	runFromTemp()

	if !ensureSingleton() {
		os.Exit(0)
	}

	loadConfig()
	systray.Run(onReady, onExit)
}

func runFromTemp() {
	const envMarker = "SSH_SOCKS_RUN_FROM_TEMP"
	if os.Getenv(envMarker) == "1" {
		return
	}

	exePath, err := os.Executable()
	if err != nil {
		return
	}
	tempDir := os.TempDir()
	if strings.HasPrefix(strings.ToLower(exePath), strings.ToLower(tempDir)) {
		return
	}

	src, err := os.Open(exePath)
	if err != nil {
		return
	}
	defer src.Close()

	dstPath := filepath.Join(tempDir, "ssh_socks.exe")
	dst, err := os.Create(dstPath)
	if err != nil {
		return
	}

	_, err = io.Copy(dst, src)
	dst.Close()
	if err != nil {
		os.Remove(dstPath)
		return
	}

	cmd := exec.Command(dstPath, os.Args[1:]...)
	cmd.Env = append(os.Environ(), envMarker+"=1")
	if err := cmd.Start(); err != nil {
		os.Remove(dstPath)
		return
	}

	time.Sleep(500 * time.Millisecond)
	os.Exit(0)
}

func ensureSingleton() bool {
	name, _ := syscall.UTF16PtrFromString("Global\\ssh_socks_single_instance_mutex")
	h, _, err := syscall.SyscallN(
		kernel32.NewProc("CreateMutexW").Addr(),
		0,
		1,
		uintptr(unsafe.Pointer(name)),
	)
	if h == 0 {
		return true
	}

	singletonHandle = syscall.Handle(h)

	if err == syscall.ERROR_ALREADY_EXISTS {
		killExisting()
		h2, _, _ := syscall.SyscallN(
			kernel32.NewProc("OpenMutexW").Addr(),
			uintptr(0x00100000),
			1,
			uintptr(unsafe.Pointer(name)),
		)
		if h2 != 0 {
			syscall.SyscallN(kernel32.NewProc("CloseHandle").Addr(), uintptr(h2))
		}
		return false
	}
	return true
}

func closeSingleton() {
	if singletonHandle != 0 {
		syscall.SyscallN(kernel32.NewProc("ReleaseMutex").Addr(), uintptr(singletonHandle))
		syscall.SyscallN(kernel32.NewProc("CloseHandle").Addr(), uintptr(singletonHandle))
		singletonHandle = 0
	}
}

func killExisting() {
	currentPID := os.Getpid()
	cmd := exec.Command("tasklist", "/FI", "IMAGENAME eq ssh_socks.exe", "/FO", "CSV", "/NH")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	if err != nil {
		return
	}

	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		fields := strings.Split(line, ",")
		if len(fields) < 2 {
			continue
		}
		pidStr := strings.Trim(fields[1], "\" ")
		var pid int
		fmt.Sscanf(pidStr, "%d", &pid)
		if pid <= 0 || pid == currentPID {
			continue
		}
		proc, err := os.FindProcess(pid)
		if err == nil {
			proc.Kill()
		}
	}
	time.Sleep(500 * time.Millisecond)
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
	mHTTPProxy = systray.AddMenuItemCheckbox("Enable HTTP Proxy", "Run HTTP proxy on configured port", conf.HTTPProxyEnabled)
	// Добавляем пункт фильтрации в меню
	mReFilter = systray.AddMenuItemCheckbox("Re-filter-lists", "Route only specific sites", conf.ReFilterEnabled)

	systray.AddSeparator()
	mSettings := systray.AddMenuItem("Settings", "Connection parameters")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Exit", "Close application")

	registerWindowClass("SSHProxySettings", syscall.NewCallback(settingsWndProc))
	registerWindowClass("SSHPassPrompt", syscall.NewCallback(passWndProc))

	setState(StateDisconnected)

	// Если при прошлом запуске фильтрация была включена, начинаем загрузку списков сразу
	if conf.ReFilterEnabled {
		go func() {
			if err := downloadFilterLists(); err != nil {
				debugLog("[Filters] Startup download failed: %v", err)
			}
		}()
	}

	if conf.AutoConnect && len(conf.Profiles) > 0 {
		go startSSHLoop()
	}

	go func() {
		for {
			select {
			case <-mConnect.ClickedCh:
				go startSSHLoop()
			case <-mDisconnect.ClickedCh:
				go func() {
					stopSSH()
					setState(StateDisconnected)
				}()
			case <-mHTTPProxy.ClickedCh:
				go handleHTTPProxyToggle()
			case <-mReFilter.ClickedCh:
				// Вызываем обработчик включения/выключения фильтра
				go handleReFilterToggle()
			case <-mSettings.ClickedCh:
				if !settingsOpen {
					go showSettingsNative()
				}
			case <-mQuit.ClickedCh:
				go onExit()
				return
			}
		}
	}()
}

func onExit() {
	stopSSH()
	closeSingleton()
	os.Exit(0) // Принудительный выход из процесса
}

func handleHTTPProxyToggle() {
	if mHTTPProxy.Checked() {
		mHTTPProxy.Uncheck()
		if httpProxyServer != nil {
			httpProxyServer.Stop()
			httpProxyServer = nil
		}
		confMutex.Lock()
		conf.HTTPProxyEnabled = false
		confMutex.Unlock()
		saveConfig()
	} else {
		mHTTPProxy.Check()
		if activeRunningProfile != nil && activeRunningProfile.HTTPPort != "" {
			bindAddr := activeRunningProfile.BindAddress
			if bindAddr == "" {
				bindAddr = "127.0.0.1"
			}
			socksAddr := net.JoinHostPort(bindAddr, activeRunningProfile.SocksPort)
			httpAddr := net.JoinHostPort(bindAddr, activeRunningProfile.HTTPPort)
			httpProxyServer = NewHTTPProxy(httpAddr, socksAddr)
			if err := httpProxyServer.Start(); err != nil {
				showError("HTTP Proxy Error", "Failed to start HTTP proxy:\n"+err.Error())
				mHTTPProxy.Uncheck()
			} else {
				confMutex.Lock()
				conf.HTTPProxyEnabled = true
				confMutex.Unlock()
				saveConfig()
			}
		}
	}
}

func handleReFilterToggle() {
	confMutex.Lock()
	conf.ReFilterEnabled = !conf.ReFilterEnabled
	isEnabled := conf.ReFilterEnabled
	confMutex.Unlock()

	if isEnabled {
		mReFilter.Check()
		go downloadFilterLists()
	} else {
		mReFilter.Uncheck()
	}
	saveConfig()

	// Полная остановка текущих процессов
	stopSSH()

	// Ждем сброса флага loopRunning в горутине, чтобы избежать гонки "Already running"
	go func() {
		// Ограничиваем ожидание 2 секундами
		for i := 0; i < 20; i++ {
			loopMutex.Lock()
			running := loopRunning
			loopMutex.Unlock()
			if !running {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		debugLog("[Filter] UI Toggle: Restarting connection...")
		startSSHLoop()
	}()
}

func rebuildServersMenu() {
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
		return fmt.Errorf("tor installation failed: %v\nOutput: %s", err, string(output))
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
H=$HOME
CFG="$H/.ssh_proxy_torrc"
PID="$H/.ssh_proxy_tor.pid"
DATA="$H/.ssh_proxy_data"
if [ -f "$PID" ]; then
    kill $(cat "$PID") > /dev/null 2>&1 || true
    rm "$PID"
fi
fuser -k %s/tcp > /dev/null 2>&1 || true
mkdir -p "$DATA"
cat <<EOF > "$CFG"
SocksPort 127.0.0.1:%s
DataDirectory $DATA
PidFile $PID
ExitNodes {%s}
StrictNodes 1
EOF
nohup tor -f "$CFG" > "$DATA/tor.log" 2>&1 &
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
		return fmt.Errorf("tor setup failed: %v\n%s", err, string(output))
	}
	return nil
}

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
	content, err := os.ReadFile(pubKey)
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
		return fmt.Errorf("authentication cancelled")
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
		return fmt.Errorf("dial error: %v", err)
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
	defer handlePanic()

	loopMutex.Lock()
	if loopRunning {
		loopMutex.Unlock()
		debugLog("[Loop] Already running, skip start")
		return
	}
	loopRunning = true
	loopMutex.Unlock()

	defer func() {
		loopMutex.Lock()
		loopRunning = false
		loopMutex.Unlock()
	}()

	// Очищаем канал от старых сигналов остановки
	for len(stopChan) > 0 {
		<-stopChan
	}

	confMutex.Lock()
	profOrigin := getActiveProfile()
	if profOrigin == nil || profOrigin.Host == "" {
		confMutex.Unlock()
		debugLog("[Loop] Error: No active profile found")
		setState(StateError)
		return
	}

	activeRunningProfile = profOrigin // Обновляем активный профиль для HTTP прокси

	prof := *profOrigin
	country := conf.Country
	filterEnabled := conf.ReFilterEnabled
	confMutex.Unlock()

	pubKey, err := ensureSSHKeys()
	if err != nil {
		debugLog("[Loop] SSH Key Error: %v", err)
		setState(StateError)
		return
	}

	systray.SetTooltip(fmt.Sprintf("Authenticating: %s...", prof.Name))
	if err := uploadKeyToServer(pubKey); err != nil {
		debugLog("[Loop] Auth Error: %v", err)
		setState(StateError)
		return
	}

	for {
		select {
		case <-stopChan:
			debugLog("[Loop] Stop signal, exiting loop")
			return
		default:
		}

		if sshCmd != nil && sshCmd.Process != nil {
			sshCmd.Process.Kill()
		}

		bindAddr := prof.BindAddress
		if bindAddr == "" {
			bindAddr = "127.0.0.1"
		}

		mainSocksPort := prof.SocksPort
		sshInternalPort := mainSocksPort

		// Динамический выбор внутреннего порта SSH, чтобы не занять HTTP порт
		if filterEnabled {
			var socksP, httpP int
			fmt.Sscanf(mainSocksPort, "%d", &socksP)
			fmt.Sscanf(prof.HTTPPort, "%d", &httpP)

			candidate := socksP + 1
			if candidate == httpP {
				candidate++
			}
			sshInternalPort = fmt.Sprintf("%d", candidate)
		}

		debugLog("[Loop] Iteration start: Main=%s, SSH_Internal=%s, Filter=%v", mainSocksPort, sshInternalPort, filterEnabled)

		// Очищаем порты от чужих процессов (безопасно для себя)
		killProcessOnPort(mainSocksPort)
		if mainSocksPort != sshInternalPort {
			killProcessOnPort(sshInternalPort)
		}

		// Короткая пауза для ОС, чтобы освободить сокеты
		time.Sleep(400 * time.Millisecond)

		var args []string
		if country == "direct" || country == "" {
			systray.SetTooltip(fmt.Sprintf("Direct: %s", prof.Name))
			args = []string{"-N", "-T", "-o", "ServerAliveInterval=60", "-o", "StrictHostKeyChecking=no", "-p", prof.SSHPort, "-D", bindAddr + ":" + sshInternalPort, fmt.Sprintf("%s@%s", prof.User, prof.Host)}
		} else {
			systray.SetTooltip(fmt.Sprintf("Tor (%s): %s", country, prof.Name))
			if err := setupRemoteTor(country); err != nil {
				debugLog("[Loop] Tor Setup Error: %v", err)
				setState(StateError)
				return
			}
			args = []string{"-N", "-T", "-o", "ServerAliveInterval=60", "-o", "StrictHostKeyChecking=no", "-p", prof.SSHPort, "-L", bindAddr + ":" + sshInternalPort + ":127.0.0.1:9060", fmt.Sprintf("%s@%s", prof.User, prof.Host)}
		}

		sshCmd = exec.Command("ssh", args...)
		sshCmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if err := sshCmd.Start(); err != nil {
			setState(StateError)
			time.Sleep(3 * time.Second)
			continue
		}

		// Если включена фильтрация, ждем прогрева туннеля и запускаем прокси-регулировщик
		if filterEnabled {
			if waitForPort(bindAddr+":"+sshInternalPort, 15*time.Second) {
				go startSmartSocksProxy(bindAddr, mainSocksPort, bindAddr+":"+sshInternalPort)
			} else {
				setState(StateError)
				time.Sleep(2 * time.Second)
				continue
			}
		}

		setState(StateConnected)

		// Перезапуск HTTP прокси при каждой итерации туннеля
		if httpProxyServer != nil {
			httpProxyServer.Stop()
			httpProxyServer = nil
		}

		confMutex.Lock()
		httpEnabled := conf.HTTPProxyEnabled
		confMutex.Unlock()

		if httpEnabled && prof.HTTPPort != "" {
			hAddr := net.JoinHostPort(bindAddr, prof.HTTPPort)
			sAddr := net.JoinHostPort(bindAddr, mainSocksPort)
			httpProxyServer = NewHTTPProxy(hAddr, sAddr)
			httpProxyServer.Start()
		}

		// Ждем либо падения процесса SSH, либо сигнала на выход
		done := make(chan error, 1)
		go func() { done <- sshCmd.Wait() }()

		select {
		case <-stopChan:
			return
		case <-done:
			setState(StateError)
			// Если соединение разорвалось, ждем 5 секунд и пробуем снова
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

	filterMutex.Lock()
	if smartSocksListener != nil {
		debugLog("[Stop] Closing SmartSocks listener...")
		smartSocksListener.Close()
		smartSocksListener = nil
	}
	filterMutex.Unlock()

	if httpProxyServer != nil {
		httpProxyServer.Stop()
		httpProxyServer = nil
	}

	if sshCmd != nil && sshCmd.Process != nil {
		debugLog("[Stop] Killing SSH process...")
		sshCmd.Process.Kill()
	}

	confMutex.Lock()
	prof := getActiveProfile()
	country := conf.Country
	confMutex.Unlock()

	if prof != nil {
		// Очищаем порты (теперь безопасно)
		killProcessOnPort(prof.SocksPort)

		if country != "direct" && country != "" && prof.Host != "" {
			go func(u, h, p string) {
				cleanup := `PID="$HOME/.ssh_proxy_tor.pid"; [ -f "$PID" ] && kill $(cat "$PID") && rm "$PID" || true`
				c := exec.Command("ssh", "-p", p, "-o", "StrictHostKeyChecking=no", fmt.Sprintf("%s@%s", u, h), cleanup)
				c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
				c.Run()
			}(prof.User, prof.Host, prof.SSHPort)
		}
	}
}

func killProcessOnPort(port string) {
	myPid := os.Getpid()

	cmd := exec.Command("cmd", "/c", fmt.Sprintf("netstat -ano | findstr :%s", port))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, _ := cmd.Output()
	lines := strings.Split(string(out), "\n")

	// Используем map, чтобы собрать уникальные PID и не спамить
	pidsToKill := make(map[string]bool)

	for _, line := range lines {
		fields := strings.Fields(line)
		// Ищем только строки, где порт указан как прослушиваемый или активный
		if len(fields) >= 5 && strings.Contains(fields[1], ":"+port) {
			pidStr := fields[4]
			if pidStr == "0" || pidStr == "" {
				continue
			}

			var pid int
			fmt.Sscanf(pidStr, "%d", &pid)

			if pid > 0 && pid != myPid {
				pidsToKill[pidStr] = true
			}
		}
	}

	// Убиваем только чужие процессы и только один раз
	for pidStr := range pidsToKill {
		debugLog("[Cleaner] Killing foreign process %s on port %s", pidStr, port)
		kill := exec.Command("taskkill", "/F", "/PID", pidStr)
		kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		kill.Run()
	}
}

func settingsWndProc(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	switch msg {
	case WmCommand:
		cmdID := wparam & 0xFFFF
		cmdEvent := (wparam >> 16) & 0xFFFF
		if cmdID == IDComboProfiles && cmdEvent == CbnSelChange {
			saveEditsToCurrentProfile()
			res, _, _ := procSendMessage.Call(hComboProfiles, CbGetCursel, 0, 0)
			editCurrentIdx = int(res)
			loadCurrentProfileToEdits()
			return 0
		}
		if cmdID == IDBtnAdd {
			saveEditsToCurrentProfile()
			newProf := ServerProfile{
				ID:          fmt.Sprintf("%d", time.Now().UnixNano()),
				Name:        "New Server",
				User:        "ubuntu",
				SSHPort:     "22",
				SocksPort:   "1080",
				HTTPPort:    "8080",
				BindAddress: "127.0.0.1",
			}
			editProfiles = append(editProfiles, newProf)
			editCurrentIdx = len(editProfiles) - 1
			refreshComboBox()
			loadCurrentProfileToEdits()
			return 0
		}
		if cmdID == IDBtnDel {
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
		if cmdID == IDButtonSave {
			saveEditsToCurrentProfile()
			confMutex.Lock()
			conf.Profiles = editProfiles
			if len(editProfiles) > 0 && editCurrentIdx >= 0 {
				conf.ActiveProfileID = editProfiles[editCurrentIdx].ID
			} else {
				conf.ActiveProfileID = ""
			}
			res, _, _ := procSendMessage.Call(hCbAuto, BmGetCheck, 0, 0)
			conf.AutoConnect = (res == BstChecked)
			conf.HTTPProxyEnabled = mHTTPProxy.Checked()
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
	case WmDestroy:
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
	case WmCommand:
		if wparam == IDButtonPass {
			tempPassword = getWinText(hPassEdit)
			passDone <- true
			procDestroyWin.Call(hwnd)
		}
	case WmDestroy:
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
	hwnd, _, _ := procCreateWindow.Call(0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)), 0x00C00000|0x00080000, 200, 200, 360, 540, 0, 0, 0, 0)
	hFont, _, _ := gdi32.NewProc("GetStockObject").Call(17)

	addLabel(hwnd, "Select Profile:", 15, 15, hFont)
	comboC, _ := syscall.UTF16PtrFromString("COMBOBOX")
	hComboProfiles, _, _ = procCreateWindow.Call(0x00000200, uintptr(unsafe.Pointer(comboC)), 0, 0x40000000|0x10000000|CbsDropdownList|0x00200000, 15, 35, 230, 200, hwnd, IDComboProfiles, 0, 0)
	procSendMessage.Call(hComboProfiles, WmSetFont, hFont, 1)

	btnC, _ := syscall.UTF16PtrFromString("BUTTON")
	tAdd, _ := syscall.UTF16PtrFromString("+")
	tDel, _ := syscall.UTF16PtrFromString("-")
	hBtnAdd, _, _ = procCreateWindow.Call(0, uintptr(unsafe.Pointer(btnC)), uintptr(unsafe.Pointer(tAdd)), 0x40000000|0x10000000, 255, 34, 30, 25, hwnd, IDBtnAdd, 0, 0)
	hBtnDel, _, _ = procCreateWindow.Call(0, uintptr(unsafe.Pointer(btnC)), uintptr(unsafe.Pointer(tDel)), 0x40000000|0x10000000, 290, 34, 30, 25, hwnd, IDBtnDel, 0, 0)
	procSendMessage.Call(hBtnAdd, WmSetFont, hFont, 1)
	procSendMessage.Call(hBtnDel, WmSetFont, hFont, 1)

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
	hEdHTTP = addEdit(hwnd, "", 15, 315, 140, hFont, 0)
	addLabel(hwnd, "Bind Address:", 185, 295, hFont)
	hEdBind = addEdit(hwnd, "", 185, 315, 145, hFont, 0)

	refreshComboBox()
	loadCurrentProfileToEdits()

	cbT, _ := syscall.UTF16PtrFromString("Auto-connect on startup")
	hCbAuto, _, _ = procCreateWindow.Call(0, uintptr(unsafe.Pointer(btnC)), uintptr(unsafe.Pointer(cbT)), 0x40000000|0x10000000|0x0003, 15, 375, 315, 25, hwnd, IDCheckbox, 0, 0)
	procSendMessage.Call(hCbAuto, WmSetFont, hFont, 1)
	if autoConn {
		procSendMessage.Call(hCbAuto, BmSetCheck, BstChecked, 0)
	}

	btnT, _ := syscall.UTF16PtrFromString("Save and Connect")
	hBtn, _, _ := procCreateWindow.Call(0, uintptr(unsafe.Pointer(btnC)), uintptr(unsafe.Pointer(btnT)), 0x40000000|0x10000000|0x0001, 15, 425, 315, 45, hwnd, IDButtonSave, 0, 0)
	procSendMessage.Call(hBtn, WmSetFont, hFont, 1)

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
	hPassEdit = addEdit(hwnd, "", 15, 40, 300, hFont, EsPassword)
	btnT, _ := syscall.UTF16PtrFromString("Confirm")
	btnC, _ := syscall.UTF16PtrFromString("BUTTON")
	hBtn, _, _ := procCreateWindow.Call(0, uintptr(unsafe.Pointer(btnC)), uintptr(unsafe.Pointer(btnT)), 0x40000000|0x10000000, 15, 80, 300, 40, hwnd, IDButtonPass, 0, 0)
	procSendMessage.Call(hBtn, WmSetFont, hFont, 1)
	procShowWindow.Call(hwnd, 1)
	messageLoop()
}

func showError(title, text string) {
	tPtr, _ := syscall.UTF16PtrFromString(title)
	txtPtr, _ := syscall.UTF16PtrFromString(text)
	procMessageBox.Call(0, uintptr(unsafe.Pointer(txtPtr)), uintptr(unsafe.Pointer(tPtr)), 0x00000010)
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
	procSendMessage.Call(h, WmSetFont, f, 1)
}

func addEdit(p uintptr, t string, x, y, w int, f uintptr, style uint32) uintptr {
	tP, _ := syscall.UTF16PtrFromString(t)
	cP, _ := syscall.UTF16PtrFromString("EDIT")
	h, _, _ := procCreateWindow.Call(0x00000200, uintptr(unsafe.Pointer(cP)), uintptr(unsafe.Pointer(tP)), 0x40000000|0x10000000|0x00800000|uintptr(style)|0x80, uintptr(x), uintptr(y), uintptr(w), 25, p, 0, 0, 0)
	procSendMessage.Call(h, WmSetFont, f, 1)
	return h
}

func getWinText(h uintptr) string {
	buf := make([]uint16, 32767)
	procGetWinText.Call(h, uintptr(unsafe.Pointer(&buf[0])), 32767)
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
		editProfiles[editCurrentIdx].HTTPPort = getWinText(hEdHTTP)
		editProfiles[editCurrentIdx].BindAddress = getWinText(hEdBind)
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
		setWinText(hEdHTTP, p.HTTPPort)
		setWinText(hEdBind, p.BindAddress)
	} else {
		setWinText(hEdName, "")
		setWinText(hEdUser, "")
		setWinText(hEdHost, "")
		setWinText(hEdSSH, "")
		setWinText(hEdSocks, "")
		setWinText(hEdHTTP, "8080")
		setWinText(hEdBind, "127.0.0.1")
	}
}

func refreshComboBox() {
	procSendMessage.Call(hComboProfiles, CbResetContent, 0, 0)
	for _, p := range editProfiles {
		ptr, _ := syscall.UTF16PtrFromString(p.Name)
		procSendMessage.Call(hComboProfiles, CbAddString, 0, uintptr(unsafe.Pointer(ptr)))
	}
	if editCurrentIdx >= 0 && editCurrentIdx < len(editProfiles) {
		procSendMessage.Call(hComboProfiles, CbSetCursel, uintptr(editCurrentIdx), 0)
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
	if len(conf.Profiles) == 0 && conf.Host != "" {
		newProf := ServerProfile{
			ID:          fmt.Sprintf("%d", time.Now().UnixNano()),
			Name:        "Default Server",
			User:        conf.User,
			Host:        conf.Host,
			SSHPort:     conf.SSHPort,
			SocksPort:   conf.SocksPort,
			HTTPPort:    "8080",
			BindAddress: "127.0.0.1",
		}
		conf.Profiles = append(conf.Profiles, newProf)
		conf.ActiveProfileID = newProf.ID
		conf.Host, conf.User, conf.SSHPort, conf.SocksPort = "", "", "", ""
		saveConfig()
	}
	for i := range conf.Profiles {
		if conf.Profiles[i].HTTPPort == "" {
			conf.Profiles[i].HTTPPort = "8080"
		}
		if conf.Profiles[i].BindAddress == "" {
			conf.Profiles[i].BindAddress = "127.0.0.1"
		}
	}
}

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

func downloadFilterLists() error {
	defer handlePanic()
	debugLog("[Filters] Starting download with Trie and caching...")

	urls := []string{
		"https://raw.githubusercontent.com/1andrevich/Re-filter-lists/refs/heads/main/domains_all.lst",
		"https://raw.githubusercontent.com/1andrevich/Re-filter-lists/refs/heads/main/community.lst",
		"https://raw.githubusercontent.com/1andrevich/Re-filter-lists/refs/heads/main/ipsum.lst",
		"https://raw.githubusercontent.com/1andrevich/Re-filter-lists/refs/heads/main/community_ips.lst",
		"https://raw.githubusercontent.com/1andrevich/Re-filter-lists/refs/heads/main/discord_ips.lst",
	}

	client := &http.Client{
		Timeout:   45 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}

	newDomains := make(map[string]struct{})
	newIPTree := newIPTrie()
	uniqueIPs := make(map[string]struct{}) // Для статистики дубликатов

	for _, url := range urls {
		fileName := filepath.Base(url)
		cachePath := getFilterCachePath(fileName)

		var reader io.Reader
		resp, err := client.Get(url)

		if err == nil && resp.StatusCode == http.StatusOK {
			bodyBytes, errRead := io.ReadAll(resp.Body)
			resp.Body.Close()
			if errRead == nil {
				os.WriteFile(cachePath, bodyBytes, 0644)
				reader = bytes.NewReader(bodyBytes)
				debugLog("[Filters] %s downloaded and cached", fileName)
			}
		} else {
			if resp != nil {
				resp.Body.Close()
			}
			cacheData, err := os.ReadFile(cachePath)
			if err != nil {
				debugLog("[Filters] Error: %s failed and no cache found", fileName)
				continue
			}
			reader = bytes.NewReader(cacheData)
			debugLog("[Filters] %s loaded from local cache", fileName)
		}

		addedInFile := 0
		rejectedInFile := 0
		totalInFile := 0

		scanner := bufio.NewScanner(reader)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			totalInFile++

			if len(line) > 255 {
				rejectedInFile++
				continue
			}

			// Проверка CIDR
			if _, ipnet, err := net.ParseCIDR(line); err == nil {
				cidr := ipnet.String()
				if _, exists := uniqueIPs[cidr]; !exists {
					uniqueIPs[cidr] = struct{}{}
					newIPTree.insert(ipnet)
					addedInFile++
				} else {
					rejectedInFile++
				}
				continue
			}

			// Проверка одиночного IP
			if ip := net.ParseIP(line); ip != nil {
				mask := 32
				if ip.To4() == nil {
					mask = 128
				}
				cidr := fmt.Sprintf("%s/%d", line, mask)
				if _, exists := uniqueIPs[cidr]; !exists {
					uniqueIPs[cidr] = struct{}{}
					_, ipnet, _ := net.ParseCIDR(cidr)
					newIPTree.insert(ipnet)
					addedInFile++
				} else {
					rejectedInFile++
				}
				continue
			}

			// Проверка домена
			domain := strings.ToLower(line)
			isValid := true
			dotCount := 0
			for _, r := range domain {
				if r == '.' {
					dotCount++
					continue
				}
				if r == '-' {
					continue
				}
				if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
					isValid = false
					break
				}
			}

			if !isValid || dotCount == 0 {
				rejectedLog("[%s] REJECT | %-30s | Reason: invalid", fileName, line)
				rejectedInFile++
			} else {
				if _, exists := newDomains[domain]; !exists {
					newDomains[domain] = struct{}{}
					addedInFile++
				} else {
					rejectedInFile++
				}
			}

			if len(newDomains)+len(uniqueIPs) > 800000 {
				debugLog("[Filters] Limit reached.")
				break
			}
		}
		debugLog("[Filters] File: %-15s | Total: %-6d | Added: %-6d | Rejected: %-6d",
			fileName, totalInFile, addedInFile, rejectedInFile)
	}

	filterMutex.Lock()
	filterDomains = newDomains
	filterIPs = newIPTree
	isListsLoaded = true
	filterMutex.Unlock()

	debugLog("[Filters] Ready. Domains: %d, IPs/Prefixes: %d", len(newDomains), len(uniqueIPs))
	return nil
}

func shouldProxy(address string) (bool, string) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}

	confMutex.Lock()
	filterEnabled := conf.ReFilterEnabled
	confMutex.Unlock()

	// Если фильтрация отключена — безусловно проксируем всё через туннель
	if !filterEnabled {
		return true, "filter_off_proxy_all"
	}

	filterMutex.RLock()
	defer filterMutex.RUnlock()

	// Если фильтрация ВКЛ, но списки еще не загружены —
	// проксируем всё, чтобы не было "обрыва" интернета на старте
	if !isListsLoaded {
		return true, "lists_loading_proxy_all"
	}

	// 1. Точный домен
	if _, ok := filterDomains[host]; ok {
		return true, fmt.Sprintf("exact_domain:%s", host)
	}

	// 2. Суффиксы/Поддомены
	parts := strings.Split(host, ".")
	for i := 0; i < len(parts)-1; i++ {
		suffix := strings.Join(parts[i:], ".")
		if _, ok := filterDomains[suffix]; ok {
			return true, fmt.Sprintf("suffix_match:%s(via_%s)", host, suffix)
		}
	}

	// 3. IP через Trie
	ip := net.ParseIP(host)
	if ip != nil {
		if filterIPs.contains(ip) {
			return true, fmt.Sprintf("ip_trie_match:%s", host)
		}
	}

	return false, ""
}

func startSmartSocksProxy(bindAddr, listenPort, sshSocksAddr string) {
	// 1. Ловушка для паник (запишет причину в crash.log если упадет)
	defer handlePanic()

	var l net.Listener
	var err error

	// 2. Попытки занять порт (защита от "Address already in use")
	for i := 0; i < 5; i++ {
		l, err = net.Listen("tcp", bindAddr+":"+listenPort)
		if err == nil {
			break
		}
		debugLog("[SmartSocks] Port %s busy, retry %d/5...", listenPort, i+1)
		time.Sleep(1 * time.Second)
	}

	if err != nil {
		debugLog("[SmartSocks] CRITICAL: FAILED to listen on %s: %v", listenPort, err)
		return
	}

	// 3. Сохраняем листенер в глобальную переменную под замком для stopSSH
	filterMutex.Lock()
	smartSocksListener = l
	filterMutex.Unlock()

	debugLog("[SmartSocks] UP and running on %s. Target SSH: %s", listenPort, sshSocksAddr)

	for {
		client, err := l.Accept()
		if err != nil {
			// Если листенер закрыт через l.Close() в stopSSH, Accept вернет ошибку.
			// Это нормальный способ остановить цикл.
			debugLog("[SmartSocks] Accept loop stopped (listener closed)")
			return
		}

		go func(c net.Conn) {
			// В каждой горутине тоже нужна защита от паник
			defer handlePanic()
			defer c.Close()

			// --- SOCKS5 Handshake ---
			buf := make([]byte, 256)
			// Читаем версию и кол-во методов
			if _, err := io.ReadFull(c, buf[:2]); err != nil {
				return
			}
			if buf[0] != 0x05 { // Только SOCKS5
				return
			}

			nMethods := int(buf[1])
			if _, err := io.ReadFull(c, buf[:nMethods]); err != nil {
				return
			}
			// Отвечаем: версия 5, аутентификация не требуется
			c.Write([]byte{0x05, 0x00})

			// --- Read Request ---
			if _, err := io.ReadFull(c, buf[:4]); err != nil {
				return
			}
			// buf[1] - команда (0x01 = CONNECT)
			// buf[3] - тип адреса (0x01=IPv4, 0x03=Domain, 0x04=IPv6)

			var host string
			switch buf[3] {
			case 0x01: // IPv4
				if _, err := io.ReadFull(c, buf[:4]); err != nil {
					return
				}
				host = net.IP(buf[:4]).String()
			case 0x03: // Domain
				if _, err := io.ReadFull(c, buf[:1]); err != nil {
					return
				}
				sz := int(buf[0])
				if _, err := io.ReadFull(c, buf[:sz]); err != nil {
					return
				}
				host = string(buf[:sz])
			case 0x04: // IPv6
				if _, err := io.ReadFull(c, buf[:16]); err != nil {
					return
				}
				host = net.IP(buf[:16]).String()
			default:
				debugLog("[SmartSocks] Unknown address type: %d", buf[3])
				return
			}

			// Читаем порт (2 байта)
			if _, err := io.ReadFull(c, buf[:2]); err != nil {
				return
			}
			port := binary.BigEndian.Uint16(buf[:2])
			address := net.JoinHostPort(host, fmt.Sprintf("%d", port))

			// --- Routing decision ---
			shouldUseProxy, reason := shouldProxy(address)

			var target net.Conn
			if shouldUseProxy {
				// Логируем в proxy.log
				proxyLog("[SOCKS5] PROXY | %-30s | Reason: %s", address, reason)

				// Подключаемся к SSH-туннелю
				baseDialer := &net.Dialer{Timeout: 10 * time.Second}
				dialer, errDialer := proxy.SOCKS5("tcp", sshSocksAddr, nil, baseDialer)
				if errDialer != nil {
					debugLog("[SmartSocks] Dialer Error: %v", errDialer)
					return
				}
				target, err = dialer.Dial("tcp", address)
			} else {
				// Прямое соединение (без логирования в proxy.log)
				target, err = net.DialTimeout("tcp", address, 10*time.Second)
			}

			if err != nil {
				debugLog("[SmartSocks] Connection failed to %s: %v", address, err)
				// Отправляем клиенту General SOCKS server failure (0x01)
				c.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
				return
			}
			defer target.Close()

			// Отправляем ответ об успешном соединении
			c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

			// --- Data Relay (Pipe) ---
			// Релей с защитой от зомби-горутин (взаимное закрытие через дедлайны)
			relay := func(dst, src net.Conn) {
				defer handlePanic()
				io.Copy(dst, src)
				dst.SetDeadline(time.Now())
				src.SetDeadline(time.Now())
			}

			go relay(target, c)
			relay(c, target)
		}(client)
	}
}

func waitForPort(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

func getFilterCachePath(filename string) string {
	dir, _ := os.UserConfigDir()
	path := filepath.Join(dir, "ssh-socks-tray", "filters")
	os.MkdirAll(path, 0755)
	return filepath.Join(path, filename+".cache")
}

// getLogPath возвращает полный путь к файлу лога в папке AppData
func getLogPath(filename string) string {
	dir, _ := os.UserConfigDir()
	path := filepath.Join(dir, "ssh-socks-tray", "logs")
	os.MkdirAll(path, 0755)
	return filepath.Join(path, filename)
}

// writeLog записывает сообщение в файл, ограничивая его размер
func writeLog(filename string, message string) {
	logMutex.Lock()
	defer logMutex.Unlock()

	path := getLogPath(filename)
	maxSize := int64(5 * 1024 * 1024) // 5 МБ

	if info, err := os.Stat(path); err == nil {
		if info.Size() > maxSize {
			os.WriteFile(path, []byte("--- Log truncated (size limit reached) ---\n"), 0644)
		}
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	timestamp := time.Now().Format("2006/01/02 15:04:05")
	f.WriteString(timestamp + " " + message + "\n")
}

func rejectedLog(format string, v ...interface{}) {
	msg := fmt.Sprintf(format, v...)
	writeLog("rejected.log", msg)
}

func debugLog(format string, v ...interface{}) {
	msg := fmt.Sprintf(format, v...)
	writeLog("debug.log", msg)
}

func proxyLog(format string, v ...interface{}) {
	msg := fmt.Sprintf(format, v...)
	writeLog("proxy.log", msg)
}

func handlePanic() {
	if r := recover(); r != nil {
		path := getLogPath("crash.log")
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		defer f.Close()

		fmt.Fprintf(f, "\n=== CRASH AT %s ===\n", time.Now().Format("2006-01-02 15:04:05"))
		fmt.Fprintf(f, "Panic: %v\n\n", r)

		buf := make([]byte, 2048)
		for {
			n := runtime.Stack(buf, true)
			if n < len(buf) {
				f.Write(buf[:n])
				break
			}
			buf = make([]byte, len(buf)*2)
		}
		f.WriteString("\n========================\n")
		os.Exit(1)
	}
}
