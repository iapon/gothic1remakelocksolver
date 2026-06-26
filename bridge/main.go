package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/jchv/go-webview2"
)

type KeyConfig struct {
	KeyPrev uint16 `json:"keyPrev"`
	KeyNext uint16 `json:"keyNext"`
	KeyDir1 uint16 `json:"keyDir1"`
	KeyDir2 uint16 `json:"keyDir2"`
	Delay   int    `json:"delay"`
	Method  int    `json:"method"`
}

type Effect struct {
	Target int `json:"target"`
	Dir    int `json:"dir"`
}

type Plate struct {
	InitPin int      `json:"initPin"`
	Effects []Effect `json:"effects"`
}

type Recipe struct {
	Name         string  `json:"name,omitempty"`
	NumPlates    int     `json:"numPlates"`
	NumPositions int     `json:"numPositions"`
	Center       int     `json:"center"`
	Plates       []Plate `json:"plates"`
}

type Step struct {
	Plate int `json:"plate"`
	Dir   int `json:"dir"`
}

type ExecuteRequest struct {
	NumPlates  int    `json:"numPlates"`
	StartPlate int    `json:"startPlate"`
	Steps      []Step `json:"steps"`
}

type FBRecipe struct {
	ID    string `json:"-"`
	Name  string `json:"name"`
	Steps []Step `json:"steps,omitempty"`
	Recipe
}

type WindowInfo struct {
	Handle uintptr
	Title  string
}

var (
	cfg    KeyConfig
	cfgMu  sync.Mutex
	wv     webview2.WebView
	stateMu sync.RWMutex
	state   Recipe
	cachedSteps []Step

	user32              = syscall.NewLazyDLL("user32.dll")
	procKeybdEvent      = user32.NewProc("keybd_event")
	procEnumWindows     = user32.NewProc("EnumWindows")
	procGetWindowTextW  = user32.NewProc("GetWindowTextW")
	procIsWindowVisible = user32.NewProc("IsWindowVisible")
	procPostMessageW    = user32.NewProc("PostMessageW")
	procSendMessageW    = user32.NewProc("SendMessageW")
	procMapVirtualKeyW  = user32.NewProc("MapVirtualKeyW")
	procSendInput       = user32.NewProc("SendInput")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procAttachThreadInput        = user32.NewProc("AttachThreadInput")
	procGetCurrentThreadId       = user32.NewProc("GetCurrentThreadId")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")

	targetWindowMu sync.Mutex
	targetWindows  []WindowInfo
	targetWindowIdx int

	execState struct {
		sync.Mutex
		running bool
		stop    bool
		step    int
		total   int
	}

	serverReady = make(chan struct{})
)

const firebaseProject = "gothic-1-remake-lockpiker"

func configFilePath() string {
	exe, err := os.Executable()
	if err != nil {
		return "bridge_config.json"
	}
	return filepath.Join(filepath.Dir(exe), "bridge_config.json")
}

func loadConfig() {
	data, err := os.ReadFile(configFilePath())
	if err != nil {
		return
	}
	json.Unmarshal(data, &cfg)
}

func saveConfig() {
	data, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(configFilePath(), data, 0644)
}

func vkToString(vk uint16) string {
	if vk >= 0x41 && vk <= 0x5A {
		return string(rune(vk))
	}
	if vk >= 0x30 && vk <= 0x39 {
		return string(rune(vk))
	}
	if vk >= 0x70 && vk <= 0x87 {
		return fmt.Sprintf("F%d", int(vk-0x70)+1)
	}
	switch vk {
	case 0x08:
		return "BS"
	case 0x0D:
		return "Enter"
	case 0x20:
		return "Space"
	case 0x25:
		return "Left"
	case 0x26:
		return "Up"
	case 0x27:
		return "Right"
	case 0x28:
		return "Down"
	}
	return fmt.Sprintf("0x%02X", vk)
}

func enumWindows() []WindowInfo {
	var result []WindowInfo
	cb := syscall.NewCallback(func(hwnd uintptr, lParam uintptr) uintptr {
		visible, _, _ := procIsWindowVisible.Call(hwnd)
		if visible == 0 {
			return 1
		}
		buf := make([]uint16, 256)
		procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), 256)
		title := syscall.UTF16ToString(buf)
		if title == "" {
			return 1
		}
		result = append(result, WindowInfo{Handle: hwnd, Title: title})
		return 1
	})
	procEnumWindows.Call(cb, 0)
	return result
}

func pressKeyTo(hwnd uintptr, vk uint16) {
	scanCode, _, _ := procMapVirtualKeyW.Call(uintptr(vk), 0)
	lParamDown := uintptr(1 | (scanCode << 16))
	lParamUp := uintptr(1 | (scanCode << 16) | (1<<30) | (1<<31))
	procPostMessageW.Call(hwnd, 0x0100, uintptr(vk), lParamDown)
	time.Sleep(30 * time.Millisecond)
	procPostMessageW.Call(hwnd, 0x0101, uintptr(vk), lParamUp)
	time.Sleep(30 * time.Millisecond)
}

func pressKeyForeground(vk uint16) {
	procKeybdEvent.Call(uintptr(vk), 0, 0, 0)
	time.Sleep(30 * time.Millisecond)
	procKeybdEvent.Call(uintptr(vk), 0, 0x0002, 0)
	time.Sleep(30 * time.Millisecond)
}

type tagINPUT struct {
	Type uint32
	Mi   [16]byte
}

func pressKeySendInput(hwnd uintptr, vk uint16) {
	targetThread, _, _ := procGetWindowThreadProcessId.Call(hwnd, 0)
	myThread, _, _ := procGetCurrentThreadId.Call()
	procAttachThreadInput.Call(myThread, targetThread, 1)

	scanCode, _, _ := procMapVirtualKeyW.Call(uintptr(vk), 0)

	var down tagINPUT
	down.Type = 1 // INPUT_KEYBOARD
	*(*uint16)(unsafe.Pointer(&down.Mi[0])) = vk
	*(*uint16)(unsafe.Pointer(&down.Mi[2])) = uint16(scanCode)
	*(*uint32)(unsafe.Pointer(&down.Mi[8])) = 0 // flags: keydown

	var up tagINPUT
	up.Type = 1
	*(*uint16)(unsafe.Pointer(&up.Mi[0])) = vk
	*(*uint16)(unsafe.Pointer(&up.Mi[2])) = uint16(scanCode)
	*(*uint32)(unsafe.Pointer(&up.Mi[8])) = 0x0002 // KEYEVENTF_KEYUP

	inputs := []tagINPUT{down, up}
	procSendInput.Call(2, uintptr(unsafe.Pointer(&inputs[0])), uintptr(unsafe.Sizeof(tagINPUT{})))

	procAttachThreadInput.Call(myThread, targetThread, 0)
	time.Sleep(30 * time.Millisecond)
}

func pressKeySendMessage(hwnd uintptr, vk uint16) {
	scanCode, _, _ := procMapVirtualKeyW.Call(uintptr(vk), 0)
	lParamDown := uintptr(1 | (scanCode << 16))
	lParamUp := uintptr(1 | (scanCode << 16) | (1<<30) | (1<<31))
	procSendMessageW.Call(hwnd, 0x0100, uintptr(vk), lParamDown)
	time.Sleep(30 * time.Millisecond)
	procSendMessageW.Call(hwnd, 0x0101, uintptr(vk), lParamUp)
	time.Sleep(30 * time.Millisecond)
}

func pressKeySys(hwnd uintptr, vk uint16) {
	scanCode, _, _ := procMapVirtualKeyW.Call(uintptr(vk), 0)
	lParamDown := uintptr(1 | (scanCode << 16))
	lParamUp := uintptr(1 | (scanCode << 16) | (1<<30) | (1<<31))
	procPostMessageW.Call(hwnd, 0x0104, uintptr(vk), lParamDown)
	time.Sleep(30 * time.Millisecond)
	procPostMessageW.Call(hwnd, 0x0105, uintptr(vk), lParamUp)
	time.Sleep(30 * time.Millisecond)
}

func solveBFS(r Recipe) ([]Step, error) {
	n := r.NumPlates
	pos := r.NumPositions
	if pos == 0 {
		pos = 7
	}
	center := r.Center
	if center == 0 {
		center = 3
	}
	if n == 0 || len(r.Plates) == 0 {
		return nil, fmt.Errorf("no plates")
	}

	init := make([]int, n)
	for i, p := range r.Plates {
		init[i] = p.InitPin
	}
	goal := make([]int, n)
	for i := range goal {
		goal[i] = center
	}
	stateKey := func(s []int) string {
		p := make([]string, len(s))
		for i, v := range s {
			p[i] = fmt.Sprintf("%d", v)
		}
		return strings.Join(p, ",")
	}
	initKey := stateKey(init)
	goalKey := stateKey(goal)
	if initKey == goalKey {
		return []Step{}, nil
	}

	type pi struct {
		prev string
		move Step
	}
	parent := map[string]*pi{initKey: nil}
	queue := [][]int{init}
	var found string

	for len(queue) > 0 && found == "" {
		var next [][]int
		for _, st := range queue {
			for p := 0; p < n; p++ {
				for _, d := range []int{1, -1} {
					ns := make([]int, n)
					copy(ns, st)
					ns[p] += d
					if ns[p] < 0 || ns[p] >= pos {
						continue
					}
					valid := true
					for _, e := range r.Plates[p].Effects {
						if e.Target == p {
							continue
						}
						ns[e.Target] += d * e.Dir
						if ns[e.Target] < 0 || ns[e.Target] >= pos {
							valid = false
							break
						}
					}
					if !valid {
						continue
					}
					nk := stateKey(ns)
					if _, ok := parent[nk]; ok {
						continue
					}
					parent[nk] = &pi{prev: stateKey(st), move: Step{Plate: p, Dir: d}}
					if nk == goalKey {
						found = nk
						break
					}
					next = append(next, ns)
				}
				if found != "" {
					break
				}
			}
		}
		queue = next
	}
	if found == "" {
		return nil, fmt.Errorf("no solution")
	}
	var path []Step
	k := found
	for parent[k] != nil {
		path = append([]Step{parent[k].move}, path...)
		k = parent[k].prev
	}
	return path, nil
}

func loadFirebaseRecipes() ([]FBRecipe, error) {
	url := fmt.Sprintf(
		"https://firestore.googleapis.com/v1/projects/%s/databases/(default)/documents/recipes?key=AIzaSyABGjHFV-5lgA6A-c_XuKmqN6ZpR0VyASo&pageSize=50",
		firebaseProject,
	)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		Documents []struct {
			Name   string `json:"name"`
			Fields struct {
				Name         struct{ StringValue string `json:"stringValue"` } `json:"name"`
				NumPlates    struct{ IntegerValue string `json:"integerValue"` } `json:"numPlates"`
				NumPositions struct{ IntegerValue string `json:"integerValue"` } `json:"numPositions"`
				Center       struct{ IntegerValue string `json:"integerValue"` } `json:"center"`
				Plates       struct {
					ArrayValue struct {
						Values []struct {
							MapValue struct {
								Fields struct {
									InitPin struct{ IntegerValue string `json:"integerValue"` } `json:"initPin"`
									Effects struct {
										ArrayValue struct {
											Values []struct {
												MapValue struct {
													Fields struct {
														Target struct{ IntegerValue string `json:"integerValue"` } `json:"target"`
														Dir    struct{ IntegerValue string `json:"integerValue"` } `json:"dir"`
													} `json:"fields"`
												} `json:"mapValue"`
											} `json:"values"`
										} `json:"arrayValue"`
									} `json:"effects"`
								} `json:"fields"`
							} `json:"mapValue"`
						} `json:"values"`
					} `json:"arrayValue"`
				} `json:"plates"`
				Steps struct {
					ArrayValue struct {
						Values []struct {
							MapValue struct {
								Fields struct {
									Plate struct{ IntegerValue string `json:"integerValue"` } `json:"plate"`
									Dir   struct{ IntegerValue string `json:"integerValue"` } `json:"dir"`
								} `json:"fields"`
							} `json:"mapValue"`
						} `json:"values"`
					} `json:"arrayValue"`
				} `json:"steps"`
			} `json:"fields"`
		} `json:"documents"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	var out []FBRecipe
	for _, doc := range result.Documents {
		var r Recipe
		r.Name = doc.Fields.Name.StringValue
		fmt.Sscanf(doc.Fields.NumPlates.IntegerValue, "%d", &r.NumPlates)
		fmt.Sscanf(doc.Fields.NumPositions.IntegerValue, "%d", &r.NumPositions)
		fmt.Sscanf(doc.Fields.Center.IntegerValue, "%d", &r.Center)
		for _, pv := range doc.Fields.Plates.ArrayValue.Values {
			var p Plate
			fmt.Sscanf(pv.MapValue.Fields.InitPin.IntegerValue, "%d", &p.InitPin)
			for _, ev := range pv.MapValue.Fields.Effects.ArrayValue.Values {
				var e Effect
				fmt.Sscanf(ev.MapValue.Fields.Target.IntegerValue, "%d", &e.Target)
				fmt.Sscanf(ev.MapValue.Fields.Dir.IntegerValue, "%d", &e.Dir)
				p.Effects = append(p.Effects, e)
			}
			r.Plates = append(r.Plates, p)
		}
		if r.NumPlates > 0 && len(r.Plates) > 0 {
			id := strings.Split(doc.Name, "/")[len(strings.Split(doc.Name, "/"))-1]
			var steps []Step
			for _, sv := range doc.Fields.Steps.ArrayValue.Values {
				var s Step
				fmt.Sscanf(sv.MapValue.Fields.Plate.IntegerValue, "%d", &s.Plate)
				fmt.Sscanf(sv.MapValue.Fields.Dir.IntegerValue, "%d", &s.Dir)
				steps = append(steps, s)
			}
			out = append(out, FBRecipe{ID: id, Name: r.Name, Steps: steps, Recipe: r})
		}
	}
	return out, nil
}

func isStopped() bool {
	execState.Lock()
	s := execState.stop
	execState.Unlock()
	return s
}

func executeSteps(req ExecuteRequest) {
	defer func() {
		execState.Lock()
		execState.running = false
		execState.Unlock()
		wv.Dispatch(func() {
			wv.Eval("window.updateExecStatus(false, '', 0, 0);")
		})
	}()

	cfgMu.Lock()
	c := cfg
	cfgMu.Unlock()

	targetWindowMu.Lock()
	var hwnd uintptr
	var hasHwnd bool
	if targetWindowIdx >= 0 && targetWindowIdx < len(targetWindows) {
		hwnd = targetWindows[targetWindowIdx].Handle
		hasHwnd = true
	}
	targetWindowMu.Unlock()

	steps := req.Steps
	total := len(steps)
	delay := time.Duration(c.Delay) * time.Millisecond
	currentPlate := req.StartPlate
	numPlates := req.NumPlates
	if numPlates <= 0 {
		stateMu.RLock()
		numPlates = state.NumPlates
		stateMu.RUnlock()
		if numPlates <= 0 {
			numPlates = 8
		}
	}

	press := func(vk uint16) {
		if hasHwnd {
			switch c.Method {
			case 1:
				pressKeySendInput(hwnd, vk)
			case 2:
				pressKeySendMessage(hwnd, vk)
			case 3:
				pressKeySys(hwnd, vk)
			default:
				pressKeyTo(hwnd, vk)
			}
		} else {
			pressKeyForeground(vk)
		}
		time.Sleep(delay)
	}
	execState.Lock()
	execState.total = total
	execState.Unlock()
	time.Sleep(500 * time.Millisecond)
	for i, step := range steps {
		execState.Lock()
		if execState.stop {
			execState.Unlock()
			return
		}
		execState.step = i + 1
		s := execState.step
		execState.Unlock()

		dirStr := "left"
		if step.Dir < 0 {
			dirStr = "right"
		}
		wv.Dispatch(func() {
			wv.Eval(fmt.Sprintf("window.updateExecStatus(true, 'Step %d/%d: P%d %s', %d, %d);", s, total, step.Plate+1, dirStr, s, total))
		})

		if step.Plate != currentPlate {
			if step.Plate > currentPlate {
				for j := 0; j < step.Plate-currentPlate; j++ {
					if isStopped() {
						return
					}
					press(c.KeyNext)
				}
			} else {
				for j := 0; j < currentPlate-step.Plate; j++ {
					if isStopped() {
						return
					}
					press(c.KeyPrev)
				}
			}
			currentPlate = step.Plate
		}
		if isStopped() {
			return
		}
		key := c.KeyDir1
		if step.Dir < 0 {
			key = c.KeyDir2
		}
		press(key)
	}
}

func startServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", withCORS(handleConfig))
	mux.HandleFunc("/api/execute", withCORS(handleExecute))
	mux.HandleFunc("/api/stop", withCORS(handleStop))
	mux.HandleFunc("/api/status", withCORS(handleStatus))
	mux.HandleFunc("/api/update", withCORS(handleUpdate))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(htmlContent))
	})
	serverReady <- struct{}{}
	log.Fatal(http.ListenAndServe(":8765", mux))
}

func withCORS(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(200)
			return
		}
		fn(w, r)
	}
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		cfgMu.Lock()
		defer cfgMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cfg)
	case "POST":
		var n KeyConfig
		if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		cfgMu.Lock()
		cfg = n
		cfgMu.Unlock()
		saveConfig()
		w.WriteHeader(200)
	}
}

func handleExecute(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		return
	}
	execState.Lock()
	if execState.running {
		execState.Unlock()
		http.Error(w, "already executing", 409)
		return
	}
	execState.running = true
	execState.stop = false
	execState.step = 0
	execState.total = 0
	execState.Unlock()
	var req ExecuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		execState.Lock()
		execState.running = false
		execState.Unlock()
		http.Error(w, err.Error(), 400)
		return
	}
	go executeSteps(req)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "executing"})
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	execState.Lock()
	execState.stop = true
	execState.Unlock()
	w.WriteHeader(200)
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	execState.Lock()
	s := map[string]interface{}{"running": execState.running, "step": execState.step, "total": execState.total}
	execState.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s)
}

func handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "GET only", 405)
		return
	}
	exeURL := r.URL.Query().Get("url")
	if exeURL == "" {
		http.Error(w, "missing url param", 400)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flusher", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sendEvent := func(event, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		flusher.Flush()
	}

	sendEvent("status", "downloading")

	exePath, err := os.Executable()
	if err != nil {
		sendEvent("error", err.Error())
		return
	}
	exeDir := filepath.Dir(exePath)
	tmpPath := filepath.Join(exeDir, "lock-picker-bridge-update.exe")

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(exeURL)
	if err != nil {
		sendEvent("error", "download: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		sendEvent("error", "download status: "+resp.Status)
		return
	}

	total := resp.ContentLength
	var downloaded int64
	buf := make([]byte, 32*1024)
	f, err := os.Create(tmpPath)
	if err != nil {
		sendEvent("error", "create: "+err.Error())
		return
	}
	var lastPct int
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			f.Write(buf[:n])
			downloaded += int64(n)
			if total > 0 {
				pct := int(downloaded * 100 / total)
				if pct != lastPct {
					lastPct = pct
					sendEvent("progress", fmt.Sprintf("%d", pct))
				}
			}
		}
		if err != nil {
			break
		}
	}
	f.Close()

	sendEvent("status", "installing")

	batPath := filepath.Join(exeDir, "update.bat")
	bat := fmt.Sprintf("@echo off\r\nping -n 3 127.0.0.1 >nul\r\n:retry\r\ncopy /y \"%s\" \"%s\"\r\nif errorlevel 1 (\r\n  ping -n 2 127.0.0.1 >nul\r\n  goto retry\r\n)\r\ndel \"%s\"\r\nstart \"\" \"%s\"\r\ndel \"%%~f0\"\r\n",
		tmpPath, exePath, tmpPath, exePath)
	os.WriteFile(batPath, []byte(bat), 0644)

	cmd := exec.Command("cmd", "/c", batPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Start()

	sendEvent("status", "restarting")

	go func() {
		time.Sleep(500 * time.Millisecond)
		os.Exit(0)
	}()
}

// --- JS-callable functions (bound via webview.Bind) ---

func jsGetConfig() KeyConfig {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return cfg
}

func jsSetConfig(c KeyConfig) {
	cfgMu.Lock()
	cfg = c
	cfgMu.Unlock()
	saveConfig()
}

func jsGetWindows() []map[string]interface{} {
	ws := enumWindows()
	targetWindowMu.Lock()
	targetWindows = ws
	targetWindowIdx = 0
	targetWindowMu.Unlock()
	var out []map[string]interface{}
	for _, w := range ws {
		out = append(out, map[string]interface{}{
			"handle": w.Handle,
			"title":  w.Title,
		})
	}
	return out
}

func jsSetTargetWindow(idx int) {
	targetWindowMu.Lock()
	targetWindowIdx = idx
	targetWindowMu.Unlock()
}

func jsExecute(steps []Step, numPlates int) map[string]interface{} {
	if len(steps) == 0 {
		return map[string]interface{}{"ok": false, "error": "No steps"}
	}
	if numPlates <= 0 {
		numPlates = 6
	}
	execState.Lock()
	if execState.running {
		execState.Unlock()
		return map[string]interface{}{"ok": false, "error": "Already executing"}
	}
	execState.running = true
	execState.stop = false
	execState.step = 0
	execState.total = 0
	execState.Unlock()
	go executeSteps(ExecuteRequest{NumPlates: numPlates, StartPlate: 0, Steps: steps})
	return map[string]interface{}{"ok": true}
}

func jsStop() {
	execState.Lock()
	execState.stop = true
	execState.Unlock()
}

func jsGetExecStatus() map[string]interface{} {
	execState.Lock()
	defer execState.Unlock()
	return map[string]interface{}{
		"running": execState.running,
		"step":    execState.step,
		"total":   execState.total,
	}
}

const AppVersion = "2.6.2"

type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func jsCheckUpdate() map[string]interface{} {
	resp, err := http.Get("https://api.github.com/repos/iapon/gothic1remakelocksolver/releases/latest")
	if err != nil {
		return map[string]interface{}{"ok": false, "error": err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var rel githubRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return map[string]interface{}{"ok": false, "error": err.Error()}
	}
	remoteVer := strings.TrimPrefix(rel.TagName, "v")
	log.Printf("[UPDATE] local=%s remote=%s localNum=%d remoteNum=%d", AppVersion, remoteVer, verToNum(AppVersion), verToNum(remoteVer))
	if verToNum(remoteVer) <= verToNum(AppVersion) {
		return map[string]interface{}{"ok": true, "update": false, "local": AppVersion, "remote": remoteVer}
	}
	var exeURL string
	for _, a := range rel.Assets {
		if strings.HasSuffix(a.Name, ".exe") {
			exeURL = a.URL
			break
		}
	}
	if exeURL == "" {
		return map[string]interface{}{"ok": false, "error": "no exe in release"}
	}
	return map[string]interface{}{
		"ok":       true,
		"update":   true,
		"version":  remoteVer,
		"exeUrl":   exeURL,
		"exeName":  rel.Assets[0].Name,
	}
}

func verToNum(v string) int {
	parts := strings.Split(v, ".")
	n := 0
	for _, p := range parts {
		var x int
		fmt.Sscanf(p, "%d", &x)
		n = n*100 + x
	}
	return n
}

func jsApplyUpdate(exeURL string) map[string]interface{} {
	return map[string]interface{}{"ok": true}
}

//go:embed bridge.html
var htmlContent string

func main() {
	cfg = KeyConfig{
		KeyPrev: 0x53,
		KeyNext: 0x57,
		KeyDir1: 0x41,
		KeyDir2: 0x44,
		Delay:   300,
		Method:  0,
	}
	loadConfig()

	state = Recipe{
		NumPlates:    6,
		NumPositions: 7,
		Center:       3,
		Plates: []Plate{
			{InitPin: 0}, {InitPin: 0}, {InitPin: 0},
			{InitPin: 0}, {InitPin: 0}, {InitPin: 0},
		},
	}

	go startServer()
	<-serverReady

	wv = webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  "Gothic 1 Remake - Lock Picker Bridge",
			Width:  960,
			Height: 720,
			Center: true,
		},
	})
	if wv == nil {
		log.Fatal("Failed to create webview. WebView2 runtime may not be installed.")
	}
	defer wv.Destroy()

	wv.Bind("getWindows", jsGetWindows)
	wv.Bind("setTargetWindow", jsSetTargetWindow)
	wv.Bind("execute", jsExecute)
	wv.Bind("stop", jsStop)
	wv.Bind("getExecStatus", jsGetExecStatus)
	wv.Bind("getConfig", jsGetConfig)
	wv.Bind("setConfig", jsSetConfig)
	wv.Bind("checkUpdate", jsCheckUpdate)
	wv.Bind("applyUpdate", jsApplyUpdate)

	wv.Navigate("http://localhost:8765/")
	wv.Run()
}
