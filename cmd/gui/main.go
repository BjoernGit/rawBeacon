package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/data/binding"
	"fyne.io/fyne/v2/widget"

	"github.com/BjoernGit/rawBeacon/internal/beacon"
)

func main() {
	// CLI flags
	useOSC := flag.Bool("osc", true, "Use enhanced OSC beacon (default: true)")
	useSimple := flag.Bool("simple", false, "Use simple text protocol beacon")
	port := flag.Int("port", 45221, "UDP port for beacon")
	multicast := flag.String("multicast", "239.18.7.42", "Multicast address")
	flag.Parse()

	// Discover a friendly local name
	name, _ := os.Hostname()
	if name == "" {
		name = "unknown"
	}

	// Config for beacon
	cfg := beacon.Config{
		Port:          *port,
		BroadcastAddr: *multicast,
		Name:          name,
		Interval:      0, // Manual mode
	}

	// --- UI setup (Fyne) ---
	a := app.New()
	w := a.NewWindow("rawBeacon OSC")
	w.Resize(fyne.NewSize(800, 600))

	// Reactive bindings
	itemsData := binding.NewStringList()
	statusB := binding.NewString()
	peersData := binding.NewStringList()
	_ = statusB.Set("Starting...")

	// Main list for messages
	messageList := widget.NewListWithData(
		itemsData,
		func() fyne.CanvasObject {
			return widget.NewLabel("")
		},
		func(di binding.DataItem, o fyne.CanvasObject) {
			s, _ := di.(binding.String).Get()
			o.(*widget.Label).SetText(s)
		},
	)

	// Peer list sidebar
	peerList := widget.NewListWithData(
		peersData,
		func() fyne.CanvasObject {
			return container.NewVBox(
				widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
				widget.NewLabel(""),
			)
		},
		func(di binding.DataItem, o fyne.CanvasObject) {
			s, _ := di.(binding.String).Get()
			parts := strings.Split(s, "|")
			if len(parts) >= 2 {
				o.(*fyne.Container).Objects[0].(*widget.Label).SetText(parts[0])
				o.(*fyne.Container).Objects[1].(*widget.Label).SetText(parts[1])
			}
		},
	)

	// Status label
	status := widget.NewLabelWithData(statusB)

	// Control buttons
	btnPing := widget.NewButton("🔊 Ping Now", nil)
	btnPing.Importance = widget.HighImportance

	btnClear := widget.NewButton("Clear Log", func() {
		itemsData.Set([]string{})
	})

	btnRefresh := widget.NewButton("🔄 Refresh Peers", nil)

	// Mode indicator
	modeLabel := widget.NewLabel("Mode: OSC Enhanced")
	if *useSimple {
		modeLabel.SetText("Mode: Simple Protocol")
	}

	// Layout
	header := container.NewBorder(
		nil, nil,
		container.NewHBox(btnPing, btnRefresh, btnClear),
		modeLabel,
	)

	// Peer panel
	peerPanel := container.NewBorder(
		widget.NewLabelWithStyle("Discovered Peers", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
		nil, nil, nil,
		container.NewScroll(peerList),
	)
	peerPanel = container.NewMax(
		widget.NewCard("", "", peerPanel),
	)

	// Main content with split view
	content := container.NewHSplit(
		container.NewBorder(
			nil,
			status,
			nil, nil,
			container.NewScroll(messageList),
		),
		peerPanel,
	)
	content.SetOffset(0.7) // 70% for messages, 30% for peers

	mainContent := container.NewBorder(
		header,
		nil, nil, nil,
		content,
	)

	w.SetContent(mainContent)

	// --- Beacon service ---
	var svc interface {
		Start(func(string, string)) error
		SendOnce() error
		Close() error
		GetPeers() []string
		GetStatus() string
	}

	var err error
	if *useSimple {
		// Use original simple beacon
		basicSvc, err := beacon.New(cfg)
		if err != nil {
			_ = statusB.Set("Startup error: " + err.Error())
			w.ShowAndRun()
			return
		}
		// Wrap it to match interface
		svc = &simpleServiceWrapper{basicSvc}
	} else {
		// Use enhanced OSC beacon
		svc, err = beacon.NewOSC(cfg)
		if err != nil {
			_ = statusB.Set("Startup error: " + err.Error())
			w.ShowAndRun()
			return
		}
	}
	defer svc.Close()

	// Button handlers
	btnPing.OnTapped = func() {
		_ = statusB.Set("Sending OSC beacon...")
		if err := svc.SendOnce(); err != nil {
			_ = statusB.Set("Send failed: " + err.Error())
			return
		}
		_ = statusB.Set("Beacon sent!")

		// Auto-refresh peer list after ping
		updatePeerList()
	}

	// Peer list updater
	updatePeerList := func() {
		peers := svc.GetPeers()
		var formatted []string
		for _, p := range peers {
			// Format: "Tag|Details"
			parts := strings.SplitN(p, " @ ", 2)
			if len(parts) == 2 {
				formatted = append(formatted, parts[0]+"|📍 "+parts[1])
			} else {
				formatted = append(formatted, p+"|")
			}
		}
		_ = peersData.Set(formatted)

		// Update status
		_ = statusB.Set(svc.GetStatus())
	}

	btnRefresh.OnTapped = updatePeerList

	// Message handler
	messageHandler := func(src, msg string) {
		// Format message for display
		var displayMsg string

		if strings.HasPrefix(msg, "DISCOVERED|") {
			parts := strings.Split(msg, "|")
			if len(parts) >= 3 {
				displayMsg = fmt.Sprintf("🟢 NEW: %s (%s) from %s", parts[1], parts[2], src)
			}
		} else if strings.HasPrefix(msg, "UPDATED|") {
			parts := strings.Split(msg, "|")
			if len(parts) >= 3 {
				displayMsg = fmt.Sprintf("🔄 UPDATE: %s (%s) from %s", parts[1], parts[2], src)
			}
		} else if strings.HasPrefix(msg, "LOST|") {
			parts := strings.Split(msg, "|")
			if len(parts) >= 2 {
				displayMsg = fmt.Sprintf("🔴 LOST: %s", parts[1])
			}
		} else if strings.Contains(msg, "/beacon/") {
			// OSC message
			displayMsg = fmt.Sprintf("📨 OSC from %s: %s", src, msg)
		} else {
			// Simple protocol message
			displayMsg = fmt.Sprintf("📦 %s → %s", src, msg)
		}

		if displayMsg != "" {
			_ = itemsData.Append(displayMsg)
		}

		// Auto-update peer list
		updatePeerList()
	}

	// Start beacon receiver in background
	go func() {
		_ = statusB.Set("Starting beacon service...")
		err := svc.Start(messageHandler)
		if err != nil {
			_ = statusB.Set("Receiver stopped: " + err.Error())
			log.Println(err)
		}
	}()

	// Initial status
	_ = statusB.Set("Ready - Listening on port " + fmt.Sprint(*port))

	// Periodic peer list refresh
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			a.SendNotification(fyne.NewNotification("", "")) // Dummy to trigger UI update
			updatePeerList()
		}
	}()

	w.ShowAndRun()
}

// Wrapper for the simple service to match the extended interface
type simpleServiceWrapper struct {
	*beacon.Service
}

func (s *simpleServiceWrapper) GetPeers() []string {
	// Simple service doesn't track peers, return empty
	return []string{}
}

func (s *simpleServiceWrapper) GetStatus() string {
	return "Simple beacon mode"
}
