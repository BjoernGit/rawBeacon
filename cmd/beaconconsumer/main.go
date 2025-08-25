package main

import (
	"context"
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

//
// -------------------------------
// Types / State
// -------------------------------
//

type Peer struct {
	UID string
	Tag string
	IP  string
	// Port int // optional (wenn du IDs/response mit Port erweiterst)
}

type Row struct {
	Kind string // "peer" oder "tag"
	UID  string // owner peer UID (bei peer = eigene UID; bei tag = Parent-UID)
	Text string // Rendertext
}

var (
	// Config / defaults
	beaconHost = "127.0.0.1" // Ziel-Beacon (Sidecar), an den wir unsere Requests schicken
	beaconPort = 47111       // Receive-Port des Ziel-Beacons
	recvPort   = 48001       // unser eigener Consumer-Listenport (Rückkanal)

	// Runtime
	stopRecv   chan struct{}
	wg         sync.WaitGroup
	logFile    *os.File
	recvLocker sync.Mutex // schützt Neustart

	// Data
	peersMu sync.Mutex
	peers   = map[string]Peer{}

	rowsMu  sync.Mutex
	rows    []Row
	rowsBnd = binding.NewStringList()

	selectedIndex = -1
)

//
// -------------------------------
// Logging helpers
// -------------------------------
//

func openLog() {
	f, err := os.OpenFile("beaconconsumer.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err == nil {
		log.SetOutput(f)
		logFile = f
		log.Println("=== BeaconConsumer started, logging to beaconconsumer.log ===")
	} else {
		fmt.Println("could not open beaconconsumer.log:", err)
	}
}

func closeLog() {
	if logFile != nil {
		_ = logFile.Close()
	}
}

//
// -------------------------------
// Row rendering helpers
// -------------------------------
//

func setRows(newRows []Row) {
	rowsMu.Lock()
	rows = newRows
	rowsMu.Unlock()

	ss := make([]string, len(newRows))
	for i, r := range newRows {
		ss[i] = r.Text
	}
	_ = rowsBnd.Set(ss)
	log.Printf("UI rows updated: %d rows", len(newRows))
}

func currentRows() []Row {
	rowsMu.Lock()
	defer rowsMu.Unlock()
	out := make([]Row, len(rows))
	copy(out, rows)
	return out
}

func renderPeersToRows() {
	peersMu.Lock()
	list := make([]Peer, 0, len(peers))
	for _, p := range peers {
		list = append(list, p)
	}
	peersMu.Unlock()

	sort.Slice(list, func(i, j int) bool { return list[i].Tag < list[j].Tag })

	rs := make([]Row, 0, len(list))
	for _, p := range list {
		txt := fmt.Sprintf("%s  (%s)  @ %s", p.Tag, p.UID, p.IP)
		rs = append(rs, Row{Kind: "peer", UID: p.UID, Text: txt})
	}
	setRows(rs)
}

func findPeerIndexForRowIndex(idx int) int {
	rs := currentRows()
	if idx < 0 || idx >= len(rs) {
		return -1
	}
	if rs[idx].Kind == "peer" {
		return idx
	}
	for i := idx - 1; i >= 0; i-- {
		if rs[i].Kind == "peer" {
			return i
		}
	}
	return -1
}

func replaceTagsUnderPeer(uid string, tags []string) {
	rs := currentRows()

	peerIdx := -1
	for i := range rs {
		if rs[i].Kind == "peer" && rs[i].UID == uid {
			peerIdx = i
			break
		}
	}
	if peerIdx < 0 {
		log.Printf("replaceTagsUnderPeer: peer %s not visible", uid)
		return
	}

	// range der existierenden Tag-Zeilen unter diesem Peer ermitteln
	start := peerIdx + 1
	end := start
	for end < len(rs) && rs[end].Kind == "tag" && rs[end].UID == uid {
		end++
	}

	newTagRows := make([]Row, 0, len(tags))
	for _, t := range tags {
		newTagRows = append(newTagRows, Row{
			Kind: "tag",
			UID:  uid,
			Text: "    - " + t,
		})
	}

	out := make([]Row, 0, len(rs)-(end-start)+len(newTagRows))
	out = append(out, rs[:start]...)
	out = append(out, newTagRows...)
	out = append(out, rs[end:]...)
	setRows(out)
	log.Printf("replaceTagsUnderPeer: peer=%s tags=%v", uid, tags)
}

//
// -------------------------------
// IP helpers (Reply-IP bestimmen)
// -------------------------------
//

func replyIPFor(_ string) string {
	ip := pickLocalIP() // deine Funktion, die LAN/Link-Local bevorzugt
	if ip == "" || ip == "0.0.0.0" {
		return "127.0.0.1"
	}
	return ip
}

func pickLocalIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "0.0.0.0"
	}
	var linkLocal string
	for _, iface := range ifaces {
		if (iface.Flags&net.FlagUp) == 0 || (iface.Flags&net.FlagLoopback) != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				ip := ipn.IP.To4()
				if ip == nil {
					continue
				}
				// private RFC1918
				if ip[0] == 10 || (ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31) || (ip[0] == 192 && ip[1] == 168) {
					return ip.String()
				}
				// link-local
				if linkLocal == "" && ip[0] == 169 && ip[1] == 254 {
					linkLocal = ip.String()
				}
			}
		}
	}
	if linkLocal != "" {
		return linkLocal
	}
	return "0.0.0.0"
}

//
// -------------------------------
/* SENDER */
// -------------------------------
//

func sendIDsRequest() {
	reqID := fmt.Sprintf("%d", time.Now().UnixNano())
	msg := osc.NewMessage("/beacon/ids/request")
	msg.Append(replyIPFor(beaconHost)) // s
	msg.Append(int32(recvPort))        // i
	msg.Append(reqID)                  // s

	data, _ := msg.MarshalBinary()
	addr := fmt.Sprintf("%s:%d", beaconHost, beaconPort)

	log.Printf("CLICK: Query IDs")
	log.Printf("SEND /beacon/ids/request reqID=%s -> %s (reply=%s:%d)", reqID, addr, replyIPFor(beaconHost), recvPort)

	ua, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		log.Printf("resolve beacon addr failed: %v", err)
		return
	}
	c, err := net.DialUDP("udp4", nil, ua)
	if err != nil {
		log.Printf("dial beacon failed: %v", err)
		return
	}
	defer c.Close()
	if _, err := c.Write(data); err != nil {
		log.Printf("write ids/request failed: %v", err)
	}
}

func sendTagsRequest(targetUIDHex string) {
	reqID := fmt.Sprintf("%d", time.Now().UnixNano())
	msg := osc.NewMessage("/beacon/tags/request")
	msg.Append(replyIPFor(beaconHost))          // s
	msg.Append(int32(recvPort))                 // i
	msg.Append(reqID)                           // s
	msg.Append(strings.TrimSpace(targetUIDHex)) // s (empty => ask beacon itself)

	data, _ := msg.MarshalBinary()
	addr := fmt.Sprintf("%s:%d", beaconHost, beaconPort)

	log.Printf("CLICK: Query Tags (selected) target_uid=%q", strings.TrimSpace(targetUIDHex))
	log.Printf("SEND /beacon/tags/request reqID=%s target_uid=%s -> %s (reply=%s:%d)",
		reqID, strings.TrimSpace(targetUIDHex), addr, replyIPFor(beaconHost), recvPort)

	ua, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		log.Printf("resolve beacon addr failed: %v", err)
		return
	}
	c, err := net.DialUDP("udp4", nil, ua)
	if err != nil {
		log.Printf("dial beacon failed: %v", err)
		return
	}
	defer c.Close()
	if _, err := c.Write(data); err != nil {
		log.Printf("write tags/request failed: %v", err)
	}
}

//
// -------------------------------
// RECEIVER (eigene Read-Schleife mit Absender-Logging)
// -------------------------------
//

func receiverLoop(stop <-chan struct{}) {
	defer wg.Done()

	log.Printf("receiver: starting on :%d ...", recvPort)

	lc := net.ListenConfig{}
	pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf(":%d", recvPort))
	if err != nil {
		log.Printf("receiver: listen error on :%d: %v", recvPort, err)
		return
	}
	udp := pc.(*net.UDPConn)

	// Dispatcher + Handler
	disp := osc.NewStandardDispatcher()

	// --- IDs/Response ---
	_ = disp.AddMsgHandler("/beacon/ids/response", func(msg *osc.Message) {
		if len(msg.Arguments) < 2 {
			log.Println("RECV /beacon/ids/response: invalid args")
			return
		}
		reqID, _ := msg.Arguments[0].(string)
		count, _ := msg.Arguments[1].(int32)
		log.Printf("RECV /beacon/ids/response reqID=%s peers=%d", reqID, count)

		newPeers := map[string]Peer{}
		for i := 0; i < int(count); i++ {
			base := 2 + i*3 // wenn du später port mitsendest: 2 + i*4
			if base+2 >= len(msg.Arguments) {
				log.Printf("ids/resp parse cutoff at i=%d", i)
				break
			}
			uidB, _ := msg.Arguments[base+0].([]byte)
			tag, _ := msg.Arguments[base+1].(string)
			ip, _ := msg.Arguments[base+2].(string)
			uid := hex.EncodeToString(uidB)
			newPeers[uid] = Peer{UID: uid, Tag: tag, IP: ip}
			log.Printf("ids/resp peer #%d: uid=%s tag=%q ip=%s", i, uid, tag, ip)
		}
		peersMu.Lock()
		peers = newPeers
		peersMu.Unlock()
		renderPeersToRows()
	})

	// --- Tags/Response ---
	_ = disp.AddMsgHandler("/beacon/tags/response", func(msg *osc.Message) {
		if len(msg.Arguments) < 6 {
			log.Println("RECV /beacon/tags/response: invalid args")
			return
		}

		uidB, _ := msg.Arguments[0].([]byte)
		uid := hex.EncodeToString(uidB)
		primary, _ := msg.Arguments[1].(string)
		ip, _ := msg.Arguments[2].(string)
		port32, _ := msg.Arguments[3].(int32)
		reqID, _ := msg.Arguments[4].(string)
		n32, _ := msg.Arguments[5].(int32)
		want := int(n32)

		tags := make([]string, 0, want)
		for i := 0; i < want && 6+i < len(msg.Arguments); i++ {
			if s, ok := msg.Arguments[6+i].(string); ok {
				tags = append(tags, s)
			}
		}

		log.Printf("RECV /beacon/tags/response reqID=%s uid=%s primary=%q ip=%s port=%d tags=%v (count=%d)",
			reqID, uid, primary, ip, int(port32), tags, len(tags))

		replaceTagsUnderPeer(uid, tags)
	})

	// Read loop (damit wir Absender sehen)
	errCh := make(chan error, 1)
	go func() {
		defer close(errCh)
		buf := make([]byte, 65507)
		for {
			_ = udp.SetReadDeadline(time.Now().Add(10 * time.Second))
			n, addr, err := udp.ReadFromUDP(buf)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					select {
					case <-stop:
						return
					default:
						continue
					}
				}
				errCh <- fmt.Errorf("ReadFromUDP: %w", err)
				return
			}

			pkt, err := osc.ParsePacket(string(buf[:n])) // <— string statt []byte (ältere API)
			if err != nil {
				log.Printf("parse error FROM %s: %v", addr.String(), err)
				continue
			}

			switch p := pkt.(type) {
			case *osc.Message:
				log.Printf("RECV %s FROM %s", p.Address, addr.String())
				disp.Dispatch(p) // <— kein Returnwert
			case *osc.Bundle:
				log.Printf("RECV bundle FROM %s", addr.String())
				disp.Dispatch(p) // <— kein Returnwert
			default:
				log.Printf("RECV unknown packet FROM %s", addr.String())
			}
		}
	}()

	// Stop / Fehler
	select {
	case <-stop:
		log.Println("receiver: stop signal -> closing socket")
		_ = udp.Close()
	case err := <-errCh:
		_ = udp.Close()
		if err != nil {
			log.Printf("receiver: error: %v", err)
		}
	}

	log.Println("consumer receiver exited")
}

//
// -------------------------------
// Cleanup
// -------------------------------
//

func cleanup() {
	log.Println("cleanup: stopping receiver...")
	if stopRecv != nil {
		select {
		case <-stopRecv:
		default:
			close(stopRecv)
		}
	}
	wg.Wait()
	log.Println("cleanup: done")
	closeLog()
}

//
// -------------------------------
// UI / main
// -------------------------------
//

func main() {
	openLog()

	// Ctrl+C
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)
	go func() { <-sigc; log.Println("signal: exit"); cleanup(); os.Exit(0) }()

	myApp := app.New()
	w := myApp.NewWindow("Beacon Consumer")
	w.Resize(fyne.NewSize(900, 520))
	w.SetCloseIntercept(func() { log.Println("CLICK: Close window"); cleanup(); os.Exit(0) })

	// --- Controls
	beaconHostEntry := widget.NewEntry()
	beaconHostEntry.SetText(beaconHost)
	beaconHostEntry.SetPlaceHolder("127.0.0.1")

	beaconPortEntry := widget.NewEntry()
	beaconPortEntry.SetText(strconv.Itoa(beaconPort))

	recvEntry := widget.NewEntry()
	recvEntry.SetText(strconv.Itoa(recvPort))

	btnApply := widget.NewButton("Apply", func() {
		log.Println("CLICK: Apply")
		beaconHost = strings.TrimSpace(beaconHostEntry.Text)
		if p, err := strconv.Atoi(beaconPortEntry.Text); err == nil && p > 0 && p < 65536 {
			beaconPort = p
		} else {
			log.Printf("Apply: invalid beaconPort %q", beaconPortEntry.Text)
		}
		if p, err := strconv.Atoi(recvEntry.Text); err == nil && p > 0 && p < 65536 {
			newPort := p
			if newPort != recvPort {
				log.Printf("Apply: change recvPort %d -> %d (restart receiver)", recvPort, newPort)
				recvLocker.Lock()
				recvPort = newPort
				if stopRecv != nil {
					close(stopRecv)
				}
				wg.Wait()
				stopRecv = make(chan struct{})
				wg.Add(1)
				go receiverLoop(stopRecv)
				recvLocker.Unlock()
			}
		} else {
			log.Printf("Apply: invalid recvPort %q", recvEntry.Text)
		}
	})

	btnQueryIDs := widget.NewButton("Query IDs", func() { sendIDsRequest() })

	btnClear := widget.NewButton("Clear List", func() {
		log.Println("CLICK: Clear List")
		peersMu.Lock()
		peers = map[string]Peer{}
		peersMu.Unlock()
		setRows(nil)
		selectedIndex = -1
	})

	btnQueryTags := widget.NewButton("Query Tags (selected)", func() {
		idx := selectedIndex
		peerIdx := findPeerIndexForRowIndex(idx)
		if peerIdx < 0 {
			log.Printf("CLICK: Query Tags -> no selection")
			return
		}
		rs := currentRows()
		uid := rs[peerIdx].UID
		log.Printf("CLICK: Query Tags -> selected uid=%s", uid)
		sendTagsRequest(uid)
	})

	// --- List
	list := widget.NewListWithData(
		rowsBnd,
		func() fyne.CanvasObject { return widget.NewLabel("") },
		func(di binding.DataItem, co fyne.CanvasObject) {
			str, _ := di.(binding.String).Get()
			co.(*widget.Label).SetText(str)
		},
	)
	list.OnSelected = func(id widget.ListItemID) {
		selectedIndex = int(id)
		rs := currentRows()
		if id >= 0 && int(id) < len(rs) {
			log.Printf("SELECT row=%d text=%q kind=%s uid=%s", id, rs[id].Text, rs[id].Kind, rs[id].UID)
		} else {
			log.Printf("SELECT row=%d (out of range)", id)
		}
	}
	list.OnUnselected = func(id widget.ListItemID) {
		if selectedIndex == int(id) {
			selectedIndex = -1
		}
		log.Printf("UNSELECT row=%d", id)
	}

	// Layout
	topRow := container.NewHBox(
		container.NewVBox(widget.NewLabel("Beacon Host"), beaconHostEntry),
		container.NewVBox(widget.NewLabel("Beacon Port"), beaconPortEntry),
		container.NewVBox(widget.NewLabel("Receive Port (this app)"), recvEntry),
		container.NewVBox(widget.NewLabel("Actions"),
			container.NewHBox(btnApply, btnQueryIDs, btnQueryTags, btnClear),
		),
	)
	w.SetContent(container.NewBorder(
		container.NewVBox(topRow, widget.NewSeparator(), widget.NewLabel("Beacons & Tags:")),
		nil, nil, nil,
		list,
	))

	// Start receiver once
	stopRecv = make(chan struct{})
	wg.Add(1)
	go receiverLoop(stopRecv)

	w.ShowAndRun()
	cleanup()
}
