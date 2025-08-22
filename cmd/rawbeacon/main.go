package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/data/binding"
	"fyne.io/fyne/v2/widget"
	"github.com/hypebeast/go-osc/osc"
)

// ---------- Types & Globals ----------

type Peer struct {
	UID      string
	Tag      string
	IP       string
	LastSeen time.Time
}

var (
	localUID []byte
	localTag = "DefaultID"
	sendPort = 47222
	recvPort = 47111

	peers   = make(map[string]Peer)
	peersMu sync.Mutex

	stopSender chan struct{}
	stopRecv   chan struct{}
	stopPrune  chan struct{}
	stopUI     chan struct{}
	wg         sync.WaitGroup

	restartMu sync.Mutex

	// UI binding (thread-safe updates without RunOnMain)
	listData = binding.NewStringList()

	// Refresh / pruning parameters
	staleAfter     = 5 * time.Second // delete entry if not seen for this duration
	uiRefreshEvery = 1 * time.Second // rebuild list to update "ago" text

	logFile *os.File

	// ---- Tags feature (right pane) ----
	tags   []string   // current tag list for this beacon instance
	tagsMu sync.Mutex // protects tags
)

// ---------- Helpers ----------

// uidHex returns the hex string for this instance UID
func uidHex() string { return hex.EncodeToString(localUID) }

// generateUID creates a random 16-byte UID
func generateUID() {
	localUID = make([]byte, 16)
	if _, err := rand.Read(localUID); err != nil {
		fmt.Println("failed to generate UID:", err)
		os.Exit(1)
	}
}

// isPrivateIPv4 returns true for RFC1918 addresses.
func isPrivateIPv4(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	switch {
	case ip4[0] == 10:
		return true
	case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
		return true
	case ip4[0] == 192 && ip4[1] == 168:
		return true
	default:
		return false
	}
}

// isLinkLocal169 returns true for 169.254.0.0/16.
func isLinkLocal169(ip net.IP) bool {
	ip4 := ip.To4()
	return ip4 != nil && ip4[0] == 169 && ip4[1] == 254
}

// containsAny checks if s contains any of the substrings (case-insensitive).
func containsAny(s string, subs []string) bool {
	ls := strings.ToLower(s)
	for _, sub := range subs {
		if strings.Contains(ls, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

// getLocalIP prefers RFC1918 private IPv4s (LAN/WLAN), then 169.254.x.x, else "0.0.0.0".
func getLocalIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "0.0.0.0"
	}
	var linkLocalCand string

	for _, iface := range ifaces {
		// skip down or loopback
		if (iface.Flags&net.FlagUp) == 0 || (iface.Flags&net.FlagLoopback) != 0 {
			continue
		}
		// heuristic: skip some virtual adapters by name
		if containsAny(iface.Name, []string{"virtual", "vethernet", "vpn", "docker", "hyper-v", "vbox", "zerotier"}) {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok || ipNet.IP == nil {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil {
				continue
			}
			if isPrivateIPv4(ip) {
				return ip.String()
			}
			if isLinkLocal169(ip) && linkLocalCand == "" {
				linkLocalCand = ip.String()
			}
		}
	}
	if linkLocalCand != "" {
		return linkLocalCand
	}
	return "0.0.0.0"
}

// refreshListBinding rebuilds and replaces the bound peer list so "[Xs ago]" updates live.
func refreshListBinding() {
	peersMu.Lock()
	now := time.Now()
	rows := make([]string, 0, len(peers))
	for _, p := range peers {
		age := now.Sub(p.LastSeen).Truncate(time.Second)
		rows = append(rows, fmt.Sprintf("%s  (%s)  @ %s   [%s ago]", p.Tag, p.UID, p.IP, age))
	}
	peersMu.Unlock()

	sort.Strings(rows)
	_ = listData.Set(rows)
}

// ---------- Tags helpers ----------

// sanitizeTag returns only ASCII letters and digits from s.
func sanitizeTag(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') ||
			(r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// addTag inserts a tag if not empty and not duplicate.
func addTag(s string) bool {
	t := sanitizeTag(s)
	if t == "" {
		return false
	}
	tagsMu.Lock()
	defer tagsMu.Unlock()
	for _, existing := range tags {
		if strings.EqualFold(existing, t) {
			return false
		}
	}
	tags = append(tags, t)
	sort.Strings(tags)
	return true
}

// removeTag deletes a tag by exact (case-insensitive) match.
func removeTag(s string) bool {
	tagsMu.Lock()
	defer tagsMu.Unlock()
	orig := len(tags)
	out := outReset(tags[:0])
	for _, v := range tags {
		if !strings.EqualFold(v, s) {
			out = append(out, v)
		}
	}
	tags = out
	return len(tags) != orig
}

// outReset is a tiny helper to reuse slice capacity.
func outReset(b []string) []string { return b[:0] }

// GetTags returns a copy of the current tag list (sorted).
func GetTags() []string {
	tagsMu.Lock()
	defer tagsMu.Unlock()
	out := make([]string, len(tags))
	copy(out, tags)
	return out
}

// ---------- Networking ----------

// senderLoop broadcasts our ID regularly to LAN broadcast and localhost
func senderLoop(stop <-chan struct{}) {
	defer wg.Done()
	defer log.Println("sender exited")

	// broadcast destination (LAN)
	bcastAddr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("255.255.255.255:%d", sendPort))
	if err != nil {
		log.Println("resolve bcast:", err)
		return
	}
	bcastConn, err := net.DialUDP("udp4", nil, bcastAddr)
	if err != nil {
		log.Println("dial bcast:", err)
		return
	}
	defer bcastConn.Close()

	// localhost destination (second instance on same machine)
	loopAddr, _ := net.ResolveUDPAddr("udp4", fmt.Sprintf("127.0.0.1:%d", sendPort))
	loopConn, err := net.DialUDP("udp4", nil, loopAddr)
	if err != nil {
		log.Println("dial loopback:", err)
		return
	}
	defer loopConn.Close()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			msg := osc.NewMessage("/beacon/id")
			msg.Append(localUID)     // b: 16 bytes UID
			msg.Append(localTag)     // s: primary tag (name)
			msg.Append(getLocalIP()) // s: ip
			data, _ := msg.MarshalBinary()

			_, _ = bcastConn.Write(data)
			_, _ = loopConn.Write(data)
		}
	}
}

// receiverLoop listens on recvPort and updates the peers map + UI binding
func receiverLoop(stop <-chan struct{}) {
	defer wg.Done()
	defer log.Println("receiver exited")

	lc := net.ListenConfig{}
	pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf(":%d", recvPort))
	if err != nil {
		log.Println("recv listen error:", err)
		return
	}
	udp := pc.(*net.UDPConn)

	disp := osc.NewStandardDispatcher()
	_ = disp.AddMsgHandler("/beacon/id", func(msg *osc.Message) {
		uidB, _ := msg.Arguments[0].([]byte)
		uid := hex.EncodeToString(uidB)
		if uid == uidHex() {
			return // ignore our own
		}
		tag, _ := msg.Arguments[1].(string)
		ip, _ := msg.Arguments[2].(string)

		peersMu.Lock()
		peers[uid] = Peer{UID: uid, Tag: tag, IP: ip, LastSeen: time.Now()}
		peersMu.Unlock()

		refreshListBinding()
	})

	// /beacon/ids/request : args = s reply_ip, i reply_port, s request_id
	_ = disp.AddMsgHandler("/beacon/ids/request", func(msg *osc.Message) {
		if len(msg.Arguments) < 3 {
			log.Println("ids/request: not enough args")
			return
		}
		replyIP, _ := msg.Arguments[0].(string)
		replyPort32, _ := msg.Arguments[1].(int32)
		reqID, _ := msg.Arguments[2].(string)
		replyPort := int(replyPort32)

		// snapshot peers map
		peersMu.Lock()
		snap := make([]Peer, 0, len(peers))
		for _, p := range peers {
			snap = append(snap, p)
		}
		peersMu.Unlock()

		// build response
		m := osc.NewMessage("/beacon/ids/response")
		m.Append(reqID)            // s: echo request id
		m.Append(int32(len(snap))) // i: count
		for _, p := range snap {
			uidBytes, _ := hex.DecodeString(p.UID) // our map stores hex; convert back to 16 bytes
			m.Append(uidBytes)                     // b: uid(16)
			m.Append(p.Tag)                        // s
			m.Append(p.IP)                         // s
		}

		// send unicast to consumer
		dst := fmt.Sprintf("%s:%d", replyIP, replyPort)
		ua, err := net.ResolveUDPAddr("udp4", dst)
		if err != nil {
			log.Println("ids/response resolve:", err)
			return
		}
		c, err := net.DialUDP("udp4", nil, ua)
		if err != nil {
			log.Println("ids/response dial:", err)
			return
		}
		defer c.Close()
		data, _ := m.MarshalBinary()
		_, _ = c.Write(data)
	})

	_ = disp.AddMsgHandler("/beacon/tags/request", func(msg *osc.Message) {
		// Expected args: s reply_ip, i reply_port, s request_id, s target_uid_hex, i max_age_ms (optional)
		if len(msg.Arguments) < 4 {
			return
		}

		replyIP, _ := msg.Arguments[0].(string)
		replyPort, _ := msg.Arguments[1].(int32)
		reqID, _ := msg.Arguments[2].(string)
		targetUID, _ := msg.Arguments[3].(string)

		// Filter: only answer if target empty or matches us
		if targetUID != "" && !strings.EqualFold(targetUID, uidHex()) {
			return
		}

		// Build response
		tags := GetTags() // rechte Seitenleiste
		m := osc.NewMessage("/beacon/tags/response")
		m.Append(localUID)         // b
		m.Append(localTag)         // s
		m.Append(getLocalIP())     // s
		m.Append(int32(recvPort))  // i
		m.Append(reqID)            // s
		m.Append(int32(len(tags))) // i
		for _, t := range tags {
			m.Append(t) // s
		}

		// Send unicast back to requester
		addr := fmt.Sprintf("%s:%d", replyIP, int(replyPort))
		udpAddr, err := net.ResolveUDPAddr("udp4", addr)
		if err != nil {
			return
		}
		conn, err := net.DialUDP("udp4", nil, udpAddr)
		if err != nil {
			return
		}
		defer conn.Close()

		data, _ := m.MarshalBinary()
		_, _ = conn.Write(data)
	})

	server := &osc.Server{Dispatcher: disp}
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(udp) }()

	select {
	case <-stop:
		_ = udp.Close() // stops Serve()
	case err := <-errCh:
		if err != nil {
			log.Println("recv Serve error:", err)
		}
	}
}

// restartNetworking stops existing loops and starts new ones with current ports
func restartNetworking() {
	restartMu.Lock()
	defer restartMu.Unlock()

	if stopSender != nil {
		close(stopSender)
	}
	if stopRecv != nil {
		close(stopRecv)
	}
	wg.Wait()

	stopSender = make(chan struct{})
	stopRecv = make(chan struct{})

	wg.Add(2)
	go senderLoop(stopSender)
	go receiverLoop(stopRecv)
}

// pruneLoop periodically removes stale peers (no update for staleAfter)
func pruneLoop(stop <-chan struct{}) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			log.Println("prune exited")
			return
		case <-t.C:
			changed := false
			cutoff := time.Now().Add(-staleAfter)
			peersMu.Lock()
			for k, p := range peers {
				if p.LastSeen.Before(cutoff) {
					delete(peers, k)
					changed = true
				}
			}
			peersMu.Unlock()
			if changed {
				refreshListBinding()
			}
		}
	}
}

// uiRefreshLoop rebuilds the list periodically so "[Xs ago]" updates live
func uiRefreshLoop(stop <-chan struct{}) {
	t := time.NewTicker(uiRefreshEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			log.Println("ui refresh exited")
			return
		case <-t.C:
			refreshListBinding()
		}
	}
}

// cleanup stops all goroutines and waits for them to exit, then closes log.
func cleanup() {
	log.Println("cleanup: stopping goroutines...")
	if stopPrune != nil {
		select {
		case <-stopPrune:
		default:
			close(stopPrune)
		}
	}
	if stopUI != nil {
		select {
		case <-stopUI:
		default:
			close(stopUI)
		}
	}
	if stopSender != nil {
		select {
		case <-stopSender:
		default:
			close(stopSender)
		}
	}
	if stopRecv != nil {
		select {
		case <-stopRecv:
		default:
			close(stopRecv)
		}
	}
	wg.Wait()
	log.Println("cleanup: all goroutines stopped")
	if logFile != nil {
		_ = logFile.Close()
	}
}

// rebuildTagsUI (top-level): rebuilds the right tag panel.
// - tagsBox: the container to fill
// - w: current window (not strictly needed here, but kept for future focus options)
// - showNewEntry: if true, show a row to add a new tag
func rebuildTagsUI(tagsBox *fyne.Container, w fyne.Window, showNewEntry bool) {
	tagsBox.Objects = nil
	tagsBox.Add(widget.NewLabel("Tags"))
	tagsBox.Add(widget.NewSeparator())

	// optional "new tag" row
	if showNewEntry {
		newEntry := widget.NewEntry()
		newEntry.SetPlaceHolder("New tag (A–Z, a–z, 0–9)")
		addBtn := widget.NewButton("Add", func() {
			_ = addTag(newEntry.Text) // sanitize & insert if valid/unique
			rebuildTagsUI(tagsBox, w, false)
		})
		cancelBtn := widget.NewButton("Cancel", func() { rebuildTagsUI(tagsBox, w, false) })
		// Enter submits too
		newEntry.OnSubmitted = func(_ string) { addBtn.OnTapped() }

		right := container.NewHBox(addBtn, cancelBtn)
		row := container.NewBorder(nil, nil, nil, right, newEntry)

		tagsBox.Add(row)
	}

	// existing tags
	for _, t := range GetTags() {
		tagLabel := widget.NewLabel(t)
		delBtn := widget.NewButton("-", func(tag string) func() {
			return func() {
				if removeTag(tag) {
					rebuildTagsUI(tagsBox, w, false)
				}
			}
		}(t))
		row := container.NewBorder(nil, nil, tagLabel, delBtn)
		tagsBox.Add(row)
	}

	// "+" at the end
	addPlus := widget.NewButton("+", func() { rebuildTagsUI(tagsBox, w, true) })
	tagsBox.Add(widget.NewSeparator())
	tagsBox.Add(addPlus)

	tagsBox.Refresh()
}

// ---------- UI / main ----------

func main() {
	// optional file logging (keeps logs when built with -H=windowsgui)
	f, err := os.OpenFile("rawbeacon.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err == nil {
		log.SetOutput(f)
		logFile = f
	} else {
		fmt.Println("could not open log file:", err)
	}

	// handle Ctrl+C / console close gracefully too
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)
	go func() {
		<-sigc
		cleanup()
		os.Exit(0)
	}()

	generateUID()

	myApp := app.New()
	w := myApp.NewWindow("rawBeacon")
	w.Resize(fyne.NewSize(920, 480))

	// Window close: stop everything first.
	w.SetCloseIntercept(func() {
		cleanup()
		os.Exit(0)
	})

	// ---------- Left pane: Beacon controls + peers ----------

	idEntry := widget.NewEntry()
	idEntry.SetText(localTag)
	sendEntry := widget.NewEntry()
	sendEntry.SetText(strconv.Itoa(sendPort))
	recvEntry := widget.NewEntry()
	recvEntry.SetText(strconv.Itoa(recvPort))

	status := widget.NewLabel("")

	updateStatus := func() {
		status.SetText(fmt.Sprintf("Broadcasting: %q @ %s → :%d   |   Listening ← :%d",
			localTag, getLocalIP(), sendPort, recvPort))
	}

	peerList := widget.NewListWithData(
		listData,
		func() fyne.CanvasObject { return widget.NewLabelWithData(binding.NewString()) },
		func(di binding.DataItem, co fyne.CanvasObject) {
			co.(*widget.Label).Bind(di.(binding.String))
		},
	)

	applyBtn := widget.NewButton("Start / Apply", func() {
		localTag = idEntry.Text
		if p, err := strconv.Atoi(sendEntry.Text); err == nil && p > 0 && p < 65536 {
			sendPort = p
		}
		if p, err := strconv.Atoi(recvEntry.Text); err == nil && p > 0 && p < 65536 {
			recvPort = p
		}
		peersMu.Lock()
		peers = make(map[string]Peer)
		peersMu.Unlock()
		refreshListBinding()
		updateStatus()
		restartNetworking()
	})

	leftForm := container.NewGridWithColumns(3,
		container.NewVBox(widget.NewLabel("ID"), idEntry),
		container.NewVBox(widget.NewLabel("Send Port"), sendEntry),
		container.NewVBox(widget.NewLabel("Receive Port"), recvEntry),
	)

	leftPane := container.NewBorder(
		container.NewVBox(leftForm, applyBtn, status, widget.NewSeparator(), widget.NewLabel("Discovered Peers:")),
		nil, nil, nil,
		peerList,
	)

	// ---------- Right pane: Tags manager ----------
	tagsBox := container.NewVBox()
	rebuildTagsUI(tagsBox, w, false) // initial UI

	// ---------- Layout: split left / right ----------
	root := container.NewHSplit(leftPane, container.NewBorder(nil, nil, nil, nil, tagsBox))
	root.Offset = 0.66
	w.SetContent(root)

	// Start networking & background loops
	restartNetworking()
	stopPrune = make(chan struct{})
	go pruneLoop(stopPrune)
	stopUI = make(chan struct{})
	go uiRefreshLoop(stopUI)
	updateStatus()

	// Run UI
	w.ShowAndRun()

	// Fallback cleanup
	cleanup()
}
