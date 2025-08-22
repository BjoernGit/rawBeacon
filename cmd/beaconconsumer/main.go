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

// --- Types ---

type Peer struct {
	UID string
	Tag string
	IP  string
}

// --- Globals ---

var (
	recvPort   = 48001       // local port to receive responses
	beaconHost = "127.0.0.1" // target beacon host
	beaconPort = 47111       // target beacon receive port

	stopRecv chan struct{}
	wg       sync.WaitGroup
	logFile  *os.File

	peersMu sync.Mutex
	peers   = map[string]Peer{}

	peerListData  = binding.NewStringList()
	replyListData = binding.NewStringList()

	selectedPeerIndex = -1 // we track selection ourselves
)

// --- Helpers ---

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

func refreshPeerListBinding() {
	peersMu.Lock()
	rows := make([]string, 0, len(peers))
	for _, p := range peers {
		rows = append(rows, fmt.Sprintf("%s  (%s)  @ %s", p.Tag, p.UID, p.IP))
	}
	peersMu.Unlock()
	sort.Strings(rows)
	_ = peerListData.Set(rows)
}

func addReplyLine(s string) {
	// Binding's Length() returns just int; easiest is: Get, append, Set.
	cur, _ := replyListData.Get()
	cur = append(cur, s)
	_ = replyListData.Set(cur)
}

// --- Receiver ---

func receiverLoop(stop <-chan struct{}) {
	defer wg.Done()
	defer log.Println("consumer receiver exited")

	lc := net.ListenConfig{}
	pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf(":%d", recvPort))
	if err != nil {
		log.Println("recv listen error:", err)
		return
	}
	udp := pc.(*net.UDPConn)

	disp := osc.NewStandardDispatcher()

	// /beacon/ids/response
	_ = disp.AddMsgHandler("/beacon/ids/response", func(msg *osc.Message) {
		if len(msg.Arguments) < 2 {
			return
		}
		reqID, _ := msg.Arguments[0].(string)
		count, _ := msg.Arguments[1].(int32)

		peersMu.Lock()
		peers = map[string]Peer{} // reset list on each full response
		for i := 0; i < int(count); i++ {
			base := 2 + i*3
			if base+2 >= len(msg.Arguments) {
				break
			}
			uidB, _ := msg.Arguments[base+0].([]byte)
			tag, _ := msg.Arguments[base+1].(string)
			ip, _ := msg.Arguments[base+2].(string)
			uid := hex.EncodeToString(uidB)
			peers[uid] = Peer{UID: uid, Tag: tag, IP: ip}
		}
		peersMu.Unlock()
		refreshPeerListBinding()
		log.Printf("ids/response (req=%s) with %d peers\n", reqID, count)
	})

	// /beacon/tags/response
	_ = disp.AddMsgHandler("/beacon/tags/response", func(msg *osc.Message) {
		if len(msg.Arguments) < 6 {
			return
		}
		uidB, _ := msg.Arguments[0].([]byte)
		uid := hex.EncodeToString(uidB)
		primary, _ := msg.Arguments[1].(string)
		ip, _ := msg.Arguments[2].(string)
		port, _ := msg.Arguments[3].(int32)
		reqID, _ := msg.Arguments[4].(string)
		n, _ := msg.Arguments[5].(int32)

		tags := make([]string, 0, n)
		for i := 0; i < int(n) && 6+i < len(msg.Arguments); i++ {
			if s, ok := msg.Arguments[6+i].(string); ok {
				tags = append(tags, s)
			}
		}
		line := fmt.Sprintf("tags/resp[%s] from %s (%s) @ %s:%d tags=%v",
			reqID, primary, uid, ip, int(port), tags)
		log.Println(line)
		addReplyLine(line)
	})

	server := &osc.Server{Dispatcher: disp}
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(udp) }()

	select {
	case <-stop:
		_ = udp.Close()
	case err := <-errCh:
		if err != nil {
			log.Println("recv Serve error:", err)
		}
	}
}

// --- Request senders (unicast to beacon) ---

func sendIDsRequest() {
	msg := osc.NewMessage("/beacon/ids/request")
	msg.Append(getLocalIP())                             // s reply_ip
	msg.Append(int32(recvPort))                          // i reply_port
	msg.Append(fmt.Sprintf("%d", time.Now().UnixNano())) // s request_id
	data, _ := msg.MarshalBinary()
	addr := fmt.Sprintf("%s:%d", beaconHost, beaconPort)
	if ua, err := net.ResolveUDPAddr("udp4", addr); err == nil {
		if c, err := net.DialUDP("udp4", nil, ua); err == nil {
			_, _ = c.Write(data)
			_ = c.Close()
		} else {
			log.Println("dial beacon:", err)
		}
	} else {
		log.Println("resolve beacon addr:", err)
	}
}

func sendTagsRequest(targetUIDHex string) {
	msg := osc.NewMessage("/beacon/tags/request")
	msg.Append(getLocalIP())                             // s reply_ip
	msg.Append(int32(recvPort))                          // i reply_port
	msg.Append(fmt.Sprintf("%d", time.Now().UnixNano())) // s request_id
	msg.Append(strings.TrimSpace(targetUIDHex))          // s target_uid_hex
	data, _ := msg.MarshalBinary()
	addr := fmt.Sprintf("%s:%d", beaconHost, beaconPort)
	if ua, err := net.ResolveUDPAddr("udp4", addr); err == nil {
		if c, err := net.DialUDP("udp4", nil, ua); err == nil {
			_, _ = c.Write(data)
			_ = c.Close()
		} else {
			log.Println("dial beacon:", err)
		}
	} else {
		log.Println("resolve beacon addr:", err)
	}
}

// --- Cleanup ---

func cleanup() {
	log.Println("consumer cleanup...")
	if stopRecv != nil {
		select {
		case <-stopRecv:
		default:
			close(stopRecv)
		}
	}
	wg.Wait()
	if logFile != nil {
		_ = logFile.Close()
	}
	log.Println("consumer cleanup done")
}

// --- UI / main ---

func main() {
	f, err := os.OpenFile("beaconconsumer.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err == nil {
		log.SetOutput(f)
		logFile = f
	} else {
		fmt.Println("could not open log file:", err)
	}

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)
	go func() { <-sigc; cleanup(); os.Exit(0) }()

	myApp := app.New()
	w := myApp.NewWindow("Beacon Consumer")
	w.Resize(fyne.NewSize(900, 520))
	w.SetCloseIntercept(func() { cleanup(); os.Exit(0) })

	// Controls
	beaconHostEntry := widget.NewEntry()
	beaconHostEntry.SetText(beaconHost)
	beaconPortEntry := widget.NewEntry()
	beaconPortEntry.SetText(strconv.Itoa(beaconPort))
	recvEntry := widget.NewEntry()
	recvEntry.SetText(strconv.Itoa(recvPort))

	btnApply := widget.NewButton("Apply", func() {
		beaconHost = strings.TrimSpace(beaconHostEntry.Text)
		if p, err := strconv.Atoi(beaconPortEntry.Text); err == nil && p > 0 && p < 65536 {
			beaconPort = p
		}
		if p, err := strconv.Atoi(recvEntry.Text); err == nil && p > 0 && p < 65536 {
			recvPort = p
			// restart receiver
			if stopRecv != nil {
				close(stopRecv)
			}
			wg.Wait()
			stopRecv = make(chan struct{})
			wg.Add(1)
			go receiverLoop(stopRecv)
		}
	})

	btnQueryIDs := widget.NewButton("Query IDs", func() { sendIDsRequest() })

	// Peer list (with selection tracking)
	peerList := widget.NewListWithData(
		peerListData,
		func() fyne.CanvasObject { return widget.NewLabel("") },
		func(di binding.DataItem, co fyne.CanvasObject) {
			str, _ := di.(binding.String).Get()
			co.(*widget.Label).SetText(str)
		},
	)
	peerList.OnSelected = func(id widget.ListItemID) {
		selectedPeerIndex = int(id)
	}
	peerList.OnUnselected = func(id widget.ListItemID) {
		if selectedPeerIndex == int(id) {
			selectedPeerIndex = -1
		}
	}

	btnQueryTags := widget.NewButton("Query Tags (selected)", func() {
		if selectedPeerIndex < 0 {
			return
		}
		rows, _ := peerListData.Get()
		if selectedPeerIndex >= len(rows) {
			return
		}
		// parse UID between parentheses
		row := rows[selectedPeerIndex]
		start := strings.Index(row, "(")
		end := strings.Index(row, ")")
		if start >= 0 && end > start {
			uid := row[start+1 : end]
			sendTagsRequest(uid)
		}
	})

	// Replies list
	replyList := widget.NewListWithData(
		replyListData,
		func() fyne.CanvasObject { return widget.NewLabel("") },
		func(di binding.DataItem, co fyne.CanvasObject) {
			str, _ := di.(binding.String).Get()
			co.(*widget.Label).SetText(str)
		},
	)

	// Layout
	topForm := container.NewGridWithColumns(4,
		container.NewVBox(widget.NewLabel("Beacon Host"), beaconHostEntry),
		container.NewVBox(widget.NewLabel("Beacon Port"), beaconPortEntry),
		container.NewVBox(widget.NewLabel("Receive Port (this app)"), recvEntry),
		container.NewVBox(widget.NewLabel("Actions"), container.NewHBox(btnApply, btnQueryIDs, btnQueryTags)),
	)

	leftPane := container.NewBorder(
		container.NewVBox(topForm, widget.NewSeparator(), widget.NewLabel("Discovered via Beacon:")),
		nil, nil, nil, peerList,
	)
	rightPane := container.NewBorder(
		container.NewVBox(widget.NewLabel("Tag Responses"), widget.NewSeparator()),
		nil, nil, nil, replyList,
	)

	root := container.NewHSplit(leftPane, rightPane)
	root.Offset = 0.5
	w.SetContent(root)

	// start receiver
	stopRecv = make(chan struct{})
	wg.Add(1)
	go receiverLoop(stopRecv)

	w.ShowAndRun()
	cleanup()
}
