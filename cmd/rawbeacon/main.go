package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/data/binding"
	"fyne.io/fyne/v2/widget"
	"github.com/hypebeast/go-osc/osc"

	// internal packages
	"github.com/BjoernGit/rawBeacon/internal/handlers"
	"github.com/BjoernGit/rawBeacon/internal/netx"
	"github.com/BjoernGit/rawBeacon/internal/proto"
	"github.com/BjoernGit/rawBeacon/internal/store"
)

// ---------- Types & Globals ----------

type PeerView struct {
	UID      string
	Tag      string
	IP       string
	LastSeen time.Time
}

var (
	// identity & ports
	localUID []byte
	localTag = "DefaultID"
	sendPort = 47111
	recvPort = 47111

	// state stores
	peerStore = store.NewPeerStore(5 * time.Second)
	tagStore  = store.NewTagStore()

	// threading
	stopSender chan struct{}
	stopRecv   chan struct{}
	stopPrune  chan struct{}
	stopUI     chan struct{}
	wg         sync.WaitGroup
	restartMu  sync.Mutex

	// UI binding (thread-safe)
	listData = binding.NewStringList()

	// Refresh / pruning parameters
	staleAfter     = 5 * time.Second // delete entry if not seen for this duration
	uiRefreshEvery = 1 * time.Second // rebuild list to update "ago" text

	logFile *os.File
)

// ---------- Helpers ----------

// generateUID creates a random 16-byte UID
func generateUID() {
	localUID = make([]byte, 16)
	if _, err := rand.Read(localUID); err != nil {
		fmt.Println("failed to generate UID:", err)
		os.Exit(1)
	}
}

// uidHex returns the hex string for this instance UID
func uidHex() string { return hex.EncodeToString(localUID) }

// refreshListBinding rebuilds and replaces the bound peer list so "[Xs ago]" updates live.
func refreshListBinding() {
	snap := peerStore.Snapshot()
	now := time.Now()
	rows := make([]string, 0, len(snap))
	for _, p := range snap {
		age := now.Sub(p.LastSeen).Truncate(time.Second)
		rows = append(rows, fmt.Sprintf("%s  (%s)  @ %s   [%s ago]", p.Tag, p.UID, p.IP, age))
	}
	sort.Strings(rows)
	_ = listData.Set(rows)
}

// ---------- Networking ----------

// senderLoop broadcasts our ID regularly to LAN broadcast and localhost
func senderLoop(stop <-chan struct{}) {
	defer wg.Done()
	defer log.Println("sender exited")

	bc, err := netx.NewBroadcaster(sendPort)
	if err != nil {
		log.Println("broadcaster init:", err)
		return
	}
	defer bc.Close()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			msg := proto.BuildID(localUID, localTag, netx.PickLocalIP(), recvPort)
			data, _ := msg.MarshalBinary()
			_ = bc.Send(data)
		}
	}
}

// receiverLoop listens on recvPort and registers OSC handlers
func receiverLoop(stop <-chan struct{}) {
	defer wg.Done()
	defer log.Println("receiver exited")

	// bind UDP
	udp, err := netx.ListenUDP(recvPort)
	if err != nil {
		log.Println("recv listen error:", err)
		return
	}

	// dispatcher with our handlers
	disp := osc.NewStandardDispatcher()
	handlers.Register(disp, peerStore, tagStore, localUID, localTag, recvPort)

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
			removed := peerStore.Prune(time.Now())
			if removed > 0 {
				refreshListBinding()
			} else {
				// still refresh to tick the "ago" text
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

// rebuildTagsUI (right tag panel)
func rebuildTagsUI(tagsBox *fyne.Container, w fyne.Window, showNewEntry bool) {
	tagsBox.Objects = nil
	tagsBox.Add(widget.NewLabel("Tags"))
	tagsBox.Add(widget.NewSeparator())

	// optional "new tag" row
	if showNewEntry {
		newEntry := widget.NewEntry()
		newEntry.SetPlaceHolder("New tag (A–Z, a–z, 0–9)")
		addBtn := widget.NewButton("Add", func() {
			_ = tagStore.Add(newEntry.Text)
			rebuildTagsUI(tagsBox, w, false)
		})
		cancelBtn := widget.NewButton("Cancel", func() { rebuildTagsUI(tagsBox, w, false) })
		newEntry.OnSubmitted = func(_ string) { addBtn.OnTapped() }

		right := container.NewHBox(addBtn, cancelBtn)
		row := container.NewBorder(nil, nil, nil, right, newEntry) // entry stretches center
		tagsBox.Add(row)
	}

	// existing tags
	for _, t := range tagStore.List() {
		tagLabel := widget.NewLabel(t)
		delBtn := widget.NewButton("-", func(tag string) func() {
			return func() {
				if tagStore.Remove(tag) {
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
			localTag, netx.PickLocalIP(), sendPort, recvPort))
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

		// Clear peers on reconfigure (optional)
		for _, p := range peerStore.Snapshot() {
			_ = p // just to force snapshot; store has no clear; we recreate it:
		}
		peerStore = store.NewPeerStore(staleAfter)

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
