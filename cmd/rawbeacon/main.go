package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strconv"
	"sync"
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
	wg         sync.WaitGroup

	restartMu sync.Mutex

	mainWindow fyne.Window

	// UI binding (thread-safe updates without RunOnMain)
	listData = binding.NewStringList()

	// Refresh / pruning parameters
	staleAfter     = 5 * time.Second // delete entry if not seen for this duration
	uiRefreshEvery = 1 * time.Second // rebuild list to update "ago" text
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

// getLocalIP returns first non-loopback IPv4 (best effort)
func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "0.0.0.0"
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return "0.0.0.0"
}

// refreshListBinding rebuilds and replaces the bound list so "[Xs ago]" updates live
func refreshListBinding() {
	peersMu.Lock()
	now := time.Now()

	// build rows text
	rows := make([]string, 0, len(peers))
	for _, p := range peers {
		age := now.Sub(p.LastSeen).Truncate(time.Second)
		rows = append(rows, fmt.Sprintf("%s  (%s)  @ %s   [%s ago]", p.Tag, p.UID, p.IP, age))
	}
	peersMu.Unlock()

	// stable order for nicer UX
	sort.Strings(rows)

	// push whole slice to binding (triggers UI update)
	_ = listData.Set(rows)
}

// ---------- Networking ----------

// senderLoop broadcasts our ID regularly to LAN broadcast and localhost
func senderLoop(stop <-chan struct{}) {
	defer wg.Done()

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
			msg.Append(localTag)     // s: tag
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

		// update list immediately on new/updated peer
		refreshListBinding()
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
			return
		case <-t.C:
			// Recompute "ago" strings and push to binding.
			// binding.StringList is thread-safe; this triggers a redraw.
			refreshListBinding()
			log.Println("ui tick")
		}
	}
}

// ---------- UI / main ----------

func main() {
	// optional file logging (keeps logs when built with -H=windowsgui)
	f, err := os.OpenFile("rawbeacon.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err == nil {
		log.SetOutput(f)
	} else {
		fmt.Println("could not open log file:", err)
	}

	generateUID()

	myApp := app.New()
	w := myApp.NewWindow("rawBeacon PoC (ID Broadcast)")
	mainWindow = w
	w.Resize(fyne.NewSize(720, 420))

	// Inputs
	idEntry := widget.NewEntry()
	idEntry.SetText(localTag)
	sendEntry := widget.NewEntry()
	sendEntry.SetText(strconv.Itoa(sendPort))
	recvEntry := widget.NewEntry()
	recvEntry.SetText(strconv.Itoa(recvPort))

	// List bound to listData (auto-updates when listData changes)
	peerList := widget.NewListWithData(
		listData,
		// create: return a label that's already binding-aware
		func() fyne.CanvasObject { return widget.NewLabelWithData(binding.NewString()) },
		// update: bind this cell to the provided DataItem (binding.String)
		func(di binding.DataItem, co fyne.CanvasObject) {
			co.(*widget.Label).Bind(di.(binding.String))
		},
	)

	applyBtn := widget.NewButton("Start / Apply", func() {
		// read inputs
		localTag = idEntry.Text
		if p, err := strconv.Atoi(sendEntry.Text); err == nil && p > 0 && p < 65536 {
			sendPort = p
		}
		if p, err := strconv.Atoi(recvEntry.Text); err == nil && p > 0 && p < 65536 {
			recvPort = p
		}

		// clear peers on reconfigure (optional)
		peersMu.Lock()
		peers = make(map[string]Peer)
		peersMu.Unlock()
		refreshListBinding()

		restartNetworking()
	})

	form := container.NewGridWithColumns(3,
		container.NewVBox(widget.NewLabel("ID / Tag"), idEntry),
		container.NewVBox(widget.NewLabel("Send Port"), sendEntry),
		container.NewVBox(widget.NewLabel("Receive Port"), recvEntry),
	)

	w.SetContent(container.NewBorder(
		container.NewVBox(form, applyBtn, widget.NewSeparator(), widget.NewLabel("Discovered Peers:")),
		nil, nil, nil,
		peerList,
	))

	// Start networking & background loops once
	restartNetworking()
	stopPrune := make(chan struct{})
	go pruneLoop(stopPrune)
	stopUI := make(chan struct{})
	go uiRefreshLoop(stopUI)

	// Run UI
	w.ShowAndRun()

	// Cleanup on exit
	close(stopPrune)
	close(stopUI)
	if stopSender != nil {
		close(stopSender)
	}
	if stopRecv != nil {
		close(stopRecv)
	}
	wg.Wait()
}
