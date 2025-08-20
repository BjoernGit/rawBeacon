package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	"github.com/hypebeast/go-osc/osc"
)

const (
	defaultSendPort = 47017
	defaultRecvPort = 47017
	ttlMS           = 30000
)

type Peer struct {
	UID      string
	Tag      string
	IP       string
	LastSeen time.Time
}

var (
	localUID   = makeUID()
	tagName    = "DefaultID"
	sendPort   = defaultSendPort
	recvPort   = defaultRecvPort
	peers      = make(map[string]Peer)
	peersMu    sync.Mutex
	stopSender = make(chan struct{})
	stopRecv   = make(chan struct{})
	wg         sync.WaitGroup
)

func main() {
	// ---- UI ----
	a := app.New()
	w := a.NewWindow("rawBeacon PoC (ID Broadcast)")

	idEntry := widget.NewEntry()
	idEntry.SetText(tagName)
	idEntry.SetPlaceHolder("ID / Tag")
	idEntry.OnChanged = func(s string) { tagName = s }

	sendEntry := widget.NewEntry()
	sendEntry.SetText(strconv.Itoa(sendPort))
	sendEntry.SetPlaceHolder("Send Port (z.B. 47017)")

	recvEntry := widget.NewEntry()
	recvEntry.SetText(strconv.Itoa(recvPort))
	recvEntry.SetPlaceHolder("Receive Port (z.B. 47018)")

	applyBtn := widget.NewButton("Start / Apply", func() {
		// parse ports
		if p, err := strconv.Atoi(sendEntry.Text); err == nil && p > 0 && p < 65536 {
			sendPort = p
		}
		if p, err := strconv.Atoi(recvEntry.Text); err == nil && p > 0 && p < 65536 {
			recvPort = p
		}
		restartNetworking()
	})

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
					age := time.Since(p.LastSeen).Truncate(time.Second)
					o.(*widget.Label).SetText(fmt.Sprintf("%s (%s) @ %s  [%s ago]", p.Tag, p.UID, p.IP, age))
					break
				}
				idx++
			}
		},
	)

	// kleines Auto-Pruning alter Einträge
	go func() {
		t := time.NewTicker(2 * time.Second)
		for range t.C {
			changed := false
			peersMu.Lock()
			for k, p := range peers {
				if time.Since(p.LastSeen) > 2*time.Minute {
					delete(peers, k)
					changed = true
				}
			}
			peersMu.Unlock()
			if changed {
				peerList.Refresh()
			}
		}
	}()

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

	w.Resize(fyne.NewSize(680, 420))
	w.Show()

	// direkt einmal starten
	restartNetworking()

	a.Run()

	// sauber stoppen
	close(stopSender)
	close(stopRecv)
	wg.Wait()
}

func restartNetworking() {
	// bestehende Loops stoppen
	closeIfOpen(&stopSender)
	stopSender = make(chan struct{})
	closeIfOpen(&stopRecv)
	stopRecv = make(chan struct{})

	// Sender & Empfänger neu starten
	wg.Add(2)
	go senderLoop(stopSender)
	go receiverLoop(stopRecv)
}

func closeIfOpen(ch *chan struct{}) {
	select {
	case <-*ch:
	default:
		close(*ch)
	}
}

func makeUID() []byte {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return buf
}

func uidHex() string { return hex.EncodeToString(localUID) }

func senderLoop(stop <-chan struct{}) {
	defer wg.Done()

	// Ziele:
	// 1) Broadcast im LAN (255.255.255.255:<sendPort>)
	bcastAddr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("255.255.255.255:%d", sendPort))
	if err != nil {
		fmt.Println("resolve bcast:", err)
		return
	}
	bcastConn, err := net.DialUDP("udp4", nil, bcastAddr)
	if err != nil {
		fmt.Println("dial bcast:", err)
		return
	}
	defer bcastConn.Close()

	// 2) Zusätzlich localhost (127.0.0.1:<sendPort>) → so können zwei Instanzen
	//    auf demselben Rechner miteinander sehen, auch wenn Broadcast lokal nicht looped.
	localAddr, _ := net.ResolveUDPAddr("udp4", fmt.Sprintf("127.0.0.1:%d", sendPort))
	localConn, err := net.DialUDP("udp4", nil, localAddr)
	if err != nil {
		fmt.Println("dial localhost:", err)
		return
	}
	defer localConn.Close()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			tsSec := int32(time.Now().Unix())
			msg := osc.NewMessage("/beacon/id")
			msg.Append(localUID)        // b uid16
			msg.Append(tagName)         // s tag
			msg.Append(getLocalIP())    // s ip (best effort)
			msg.Append(int32(recvPort)) // i port (auf dem WIR lauschen)
			msg.Append(int32(ttlMS))    // i ttl_ms
			msg.Append(tsSec)           // i ts (sekunden)
			data, _ := msg.MarshalBinary()

			// senden an Broadcast + localhost
			_, _ = bcastConn.Write(data)
			_, _ = localConn.Write(data)
		}
	}
}

func receiverLoop(stop <-chan struct{}) {
	defer wg.Done()

	addr := &net.UDPAddr{IP: net.IPv4zero, Port: recvPort}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		fmt.Println("recv listen error:", err)
		return
	}

	disp := osc.NewStandardDispatcher()
	_ = disp.AddMsgHandler("/beacon/id", func(msg *osc.Message) {
		uidBytes, _ := msg.Arguments[0].([]byte)
		uid := hex.EncodeToString(uidBytes)
		tag, _ := msg.Arguments[1].(string)
		ip, _ := msg.Arguments[2].(string)

		if uid == uidHex() {
			return
		}
		peersMu.Lock()
		peers[uid] = Peer{UID: uid, Tag: tag, IP: ip, LastSeen: time.Now()}
		peersMu.Unlock()
	})

	server := &osc.Server{Dispatcher: disp}

	errCh := make(chan error, 1)
	go func() {
		// wichtig: Serve auf unserer UDPConn → blockiert bis conn.Close()
		errCh <- server.Serve(conn)
	}()

	for {
		select {
		case <-stop:
			// so stoppen wir den Receiver: UDPConn schließen
			_ = conn.Close()
			return
		case err := <-errCh:
			if err != nil {
				fmt.Println("recv Serve error:", err)
			}
			return
		}
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
