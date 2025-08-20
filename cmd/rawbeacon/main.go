package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	"github.com/hypebeast/go-osc/osc"
)

const (
	broadcastAddr = "255.255.255.255:47017"
	listenPort    = 47017
	ttlMS         = 30000
)

type Peer struct {
	UID      string
	Tag      string
	IP       string
	LastSeen time.Time
}

var (
	localUID = makeUID()
	tagName  = "DefaultID"
	peers    = make(map[string]Peer)
	peersMu  sync.Mutex
)

func main() {
	// --- Fyne UI ---
	a := app.New()
	w := a.NewWindow("rawBeacon PoC")

	tagEntry := widget.NewEntry()
	tagEntry.SetText(tagName)
	tagEntry.SetPlaceHolder("Enter ID/Tag here")
	tagEntry.OnChanged = func(s string) { tagName = s }

	peerList := widget.NewList(
		func() int {
			peersMu.Lock()
			defer peersMu.Unlock()
			return len(peers)
		},
		func() fyne.CanvasObject { return widget.NewLabel("peer") },
		func(i widget.ListItemID, o fyne.CanvasObject) {
			peersMu.Lock()
			defer peersMu.Unlock()
			idx := 0
			for _, p := range peers {
				if idx == i {
					o.(*widget.Label).SetText(fmt.Sprintf("%s (%s) @ %s", p.Tag, p.UID, p.IP))
					break
				}
				idx++
			}
		},
	)

	w.SetContent(container.NewVBox(
		widget.NewLabel("Local ID/Tag:"),
		tagEntry,
		widget.NewLabel("Discovered Peers:"),
		peerList,
	))
	w.Resize(fyne.NewSize(500, 400))

	// --- Networking ---
	go senderLoop()
	go receiverLoop(peerList)

	w.ShowAndRun()
}

func makeUID() []byte {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return buf
}

func uidHex() string { return hex.EncodeToString(localUID) }

func senderLoop() {
	addr, err := net.ResolveUDPAddr("udp4", broadcastAddr)
	if err != nil {
		panic(err)
	}
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	for {
		// Timestamp als int32 Sekunden (einfacher; int64 kann je nach Lib zicken)
		tsSec := int32(time.Now().Unix())

		msg := osc.NewMessage("/beacon/id")
		msg.Append(localUID)          // b uid16
		msg.Append(tagName)           // s tag
		msg.Append(getLocalIP())      // s ip
		msg.Append(int32(listenPort)) // i port
		msg.Append(int32(ttlMS))      // i ttl_ms
		msg.Append(tsSec)             // i ts (sekunden)

		data, _ := msg.MarshalBinary()
		_, _ = conn.Write(data)

		time.Sleep(2 * time.Second)
	}
}

func receiverLoop(peerList *widget.List) {
	addr := fmt.Sprintf(":%d", listenPort)

	// Dispatcher statt server.Handle(...)
	disp := osc.NewStandardDispatcher()
	_ = disp.AddMsgHandler("/beacon/id", func(msg *osc.Message) {
		uidBytes, _ := msg.Arguments[0].([]byte)
		uid := hex.EncodeToString(uidBytes)
		tag, _ := msg.Arguments[1].(string)
		ip, _ := msg.Arguments[2].(string)

		if uid == uidHex() {
			return
		} // eigenes ignorieren

		peersMu.Lock()
		peers[uid] = Peer{UID: uid, Tag: tag, IP: ip, LastSeen: time.Now()}
		peersMu.Unlock()

		peerList.Refresh()
	})

	server := &osc.Server{
		Addr:       addr,
		Dispatcher: disp,
	}

	fmt.Println("Listening on", addr)
	if err := server.ListenAndServe(); err != nil {
		fmt.Println("Error:", err)
		os.Exit(1)
	}
}

func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "0.0.0.0"
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return "0.0.0.0"
}
