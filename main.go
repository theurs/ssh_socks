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

type Config struct {
	User        string `json:"user"`
	Host        string `json:"host"`
	SSHPort     string `json:"ssh_port"`
	SocksPort   string `json:"socks_port"`
	AutoConnect bool   `json:"auto_connect"`
	Country     string `json:"country"`
}

var countries = []struct {
	ID   string
	Name string
}{
	{"direct", "🚀 Direct Connection (SSH)"},
	{"us", "🇺🇸 USA (United States)"},
	{"ca", "🇨🇦 Canada"},
	{"de", "🇩🇪 Germany"},
	{"nl", "🇳🇱 Netherlands"},
	{"fr", "🇫🇷 France"},
	{"gb", "🇬🇧 United Kingdom"},
	{"jp", "🇯🇵 Japan"},
	{"sg", "🇸🇬 Singapore"},
	{"pl", "🇵🇱 Poland"},
	{"fi", "🇫🇮 Finland"},
	{"au", "🇦🇺 Australia"},
	{"br", "🇧🇷 Brazil"},
	{"in", "🇮🇳 India"},
	{"it", "🇮🇹 Italy"},
	{"es", "🇪🇸 Spain"},
	{"se", "🇸🇪 Sweden"},
	{"no", "🇳🇴 Norway"},
	{"ch", "🇨🇭 Switzerland"},
	{"ua", "🇺🇦 Ukraine"},
	{"kr", "🇰🇷 South Korea"},
	{"hk", "🇭🇰 Hong Kong"},
}

var countryMenuItems = make(map[string]*systray.MenuItem)

var (
	conf      Config
	confMutex sync.Mutex
	sshCmd    *exec.Cmd
	stopChan  = make(chan bool, 1)
	passDone  = make(chan bool, 1)

	mConnect, mDisconnect *systray.MenuItem

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

	// Window Handles
	hEdUser, hEdHost, hEdSSH, hEdSocks, hCbAuto uintptr
	hPassEdit                                   uintptr
	tempPassword                                string
	settingsOpen                                bool
	loopRunning                                 bool
	loopMutex                                   sync.Mutex
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

	ID_BUTTON_SAVE = 1
	ID_BUTTON_PASS = 2
	ID_CHECKBOX    = 3
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
	mSettings := systray.AddMenuItem("Settings", "Connection parameters")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Exit", "Close application")

	registerWindowClass("SSHProxySettings", syscall.NewCallback(settingsWndProc))
	registerWindowClass("SSHPassPrompt", syscall.NewCallback(passWndProc))

	setState(StateDisconnected)

	if conf.AutoConnect && conf.Host != "" {
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
	user, host, port := conf.User, conf.Host, conf.SSHPort
	confMutex.Unlock()

	if host == "" {
		return fmt.Errorf("please configure server connection first")
	}

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
	user, host, port := conf.User, conf.Host, conf.SSHPort
	confMutex.Unlock()

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
	user, host, port := conf.User, conf.Host, conf.SSHPort
	confMutex.Unlock()

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

	pubKey, err := ensureSSHKeys()
	if err != nil {
		showError("Key Error", "Failed to create local SSH key: "+err.Error())
		setState(StateError)
		return
	}

	setState(StateDisconnected)
	systray.SetTooltip("Status: Checking authorization...")

	if err := uploadKeyToServer(pubKey); err != nil {
		showError("SSH Error", "Failed to authorize on server: "+err.Error())
		setState(StateError)
		return
	}

	for {
		confMutex.Lock()
		c := conf
		confMutex.Unlock()

		if sshCmd != nil && sshCmd.Process != nil {
			sshCmd.Process.Kill()
		}
		killProcessOnPort(c.SocksPort)
		time.Sleep(500 * time.Millisecond)

		var args []string

		if c.Country == "direct" || c.Country == "" {
			systray.SetTooltip("Status: Direct connection...")
			args = []string{"-N", "-T", "-o", "ServerAliveInterval=60", "-o", "StrictHostKeyChecking=no", "-p", c.SSHPort, "-D", c.SocksPort, fmt.Sprintf("%s@%s", c.User, c.Host)}
		} else {
			systray.SetTooltip("Status: Starting Tor (" + c.Country + ")...")
			if err := setupRemoteTor(c.Country); err != nil {
				showError("Tor Error", err.Error())
				setState(StateError)
				return
			}

			systray.SetTooltip("Status: Tunnel via Tor (" + c.Country + ")...")
			args = []string{"-N", "-T", "-o", "ServerAliveInterval=60", "-o", "StrictHostKeyChecking=no", "-p", c.SSHPort, "-L", c.SocksPort + ":127.0.0.1:9060", fmt.Sprintf("%s@%s", c.User, c.Host)}
		}

		sshCmd = exec.Command("ssh", args...)
		sshCmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

		if err := sshCmd.Start(); err != nil {
			setState(StateError)
			return
		}

		setState(StateConnected)
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
	if sshCmd != nil && sshCmd.Process != nil {
		sshCmd.Process.Kill()
	}

	confMutex.Lock()
	p := conf.SocksPort
	h := conf.Host
	user := conf.User
	sshP := conf.SSHPort
	country := conf.Country
	confMutex.Unlock()

	killProcessOnPort(p)

	// Cleanup on server
	if country != "direct" && country != "" && h != "" {
		go func(u, host, port string) {
			// Use PID file for targeted process termination
			cleanup := `PID="$HOME/.ssh_proxy_tor.pid"; [ -f "$PID" ] && kill $(cat "$PID") && rm "$PID" || true`
			c := exec.Command("ssh", "-p", port, "-o", "StrictHostKeyChecking=no", fmt.Sprintf("%s@%s", u, host), cleanup)
			c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
			c.Run()
		}(user, h, sshP)
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
		if wparam == ID_BUTTON_SAVE {
			confMutex.Lock()
			conf.User = getWinText(hEdUser)
			conf.Host = getWinText(hEdHost)
			conf.SSHPort = getWinText(hEdSSH)
			conf.SocksPort = getWinText(hEdSocks)
			res, _, _ := procSendMessage.Call(hCbAuto, BM_GETCHECK, 0, 0)
			conf.AutoConnect = (res == BST_CHECKED)
			confMutex.Unlock()
			saveConfig()

			stopSSH()
			go startSSHLoop()

			procDestroyWin.Call(hwnd)
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

	title, _ := syscall.UTF16PtrFromString("SSH Tunnel Settings")
	class, _ := syscall.UTF16PtrFromString("SSHProxySettings")
	hwnd, _, _ := procCreateWindow.Call(0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)), 0x00C00000|0x00080000, 200, 200, 360, 400, 0, 0, 0, 0)

	hFont, _, _ := gdi32.NewProc("GetStockObject").Call(17)

	addLabel(hwnd, "Remote User:", 15, 15, hFont)
	hEdUser = addEdit(hwnd, conf.User, 15, 35, 315, hFont, 0)

	addLabel(hwnd, "Server Host (IP):", 15, 75, hFont)
	hEdHost = addEdit(hwnd, conf.Host, 15, 95, 315, hFont, 0)

	addLabel(hwnd, "SSH Port:", 15, 135, hFont)
	hEdSSH = addEdit(hwnd, conf.SSHPort, 15, 155, 140, hFont, 0)

	addLabel(hwnd, "SOCKS Port (Local):", 185, 135, hFont)
	hEdSocks = addEdit(hwnd, conf.SocksPort, 185, 155, 145, hFont, 0)

	cbT, _ := syscall.UTF16PtrFromString("Auto-connect on startup")
	cbC, _ := syscall.UTF16PtrFromString("BUTTON")
	hCbAuto, _, _ = procCreateWindow.Call(0, uintptr(unsafe.Pointer(cbC)), uintptr(unsafe.Pointer(cbT)), 0x40000000|0x10000000|0x0003, 15, 210, 315, 25, hwnd, ID_CHECKBOX, 0, 0)
	procSendMessage.Call(hCbAuto, WM_SETFONT, hFont, 1)
	if conf.AutoConnect {
		procSendMessage.Call(hCbAuto, BM_SETCHECK, BST_CHECKED, 0)
	}

	btnT, _ := syscall.UTF16PtrFromString("Save and Connect")
	btnC, _ := syscall.UTF16PtrFromString("BUTTON")
	hBtn, _, _ := procCreateWindow.Call(0, uintptr(unsafe.Pointer(btnC)), uintptr(unsafe.Pointer(btnT)), 0x40000000|0x10000000|0x0001, 15, 270, 315, 45, hwnd, ID_BUTTON_SAVE, 0, 0)
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
		conf = Config{User: "ubuntu", Host: "", SSHPort: "22", SocksPort: "1080", AutoConnect: false, Country: "direct"}
	}
}

func saveConfig() {
	dir, _ := os.UserConfigDir()
	path := filepath.Join(dir, "ssh-socks-tray")
	os.MkdirAll(path, 0755)
	b, _ := json.MarshalIndent(conf, "", "  ")
	os.WriteFile(filepath.Join(path, "config.json"), b, 0644)
}
