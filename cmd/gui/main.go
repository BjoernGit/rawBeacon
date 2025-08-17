package main

import (
	"log"
	"os"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/data/binding"
	"fyne.io/fyne/v2/widget"

	"github.com/BjoernGit/rawBeacon/internal/beacon"
)

func main() {
	// Discover a friendly local name (hostname fallback).
	name, _ := os.Hostname()
	if name == "" {
		name = "unknown"
	}

	// Manual send mode config (Interval unused).
	cfg := beacon.Config{
		Port:          45221,
		BroadcastAddr: "255.255.255.255",
		Name:          name,
		Interval:      0,
	}

	// --- UI setup (Fyne) ---
	a := app.New()
	w := a.NewWindow("rawBeacon UI")
	w.Resize(fyne.NewSize(640, 420))

	// Reactive bindings (thread-safe updates from goroutines).
	itemsData := binding.NewStringList() // backs the List widget
	statusB := binding.NewString()
	_ = statusB.Set("Ready")

	// List bound to itemsData.
	list := widget.NewListWithData(
		itemsData,
		func() fyne.CanvasObject { return widget.NewLabel("") },
		func(di binding.DataItem, o fyne.CanvasObject) {
			s, _ := di.(binding.String).Get()
			o.(*widget.Label).SetText(s)
		},
	)

	// Status label bound to statusB.
	status := widget.NewLabelWithData(statusB)

	// Buttons.
	btnPing := widget.NewButton("Ping now", nil)

	header := container.NewHBox(btnPing, widget.NewLabel("  Send a single broadcast"))
	content := container.NewBorder(header, status, nil, nil, list)
	w.SetContent(content)

	// --- Beacon service ---
	svc, err := beacon.New(cfg)
	if err != nil {
		_ = statusB.Set("Startup error: " + err.Error())
		w.ShowAndRun()
		return
	}
	defer svc.Close()

	// Button: trigger a single outbound beacon.
	btnPing.OnTapped = func() {
		_ = statusB.Set("Sending…")
		if err := svc.SendOnce(); err != nil {
			_ = statusB.Set("Send failed: " + err.Error())
			return
		}
		_ = statusB.Set("Ping sent")
	}

	// Receive loop in background; bindings are safe across goroutines.
	go func() {
		err := svc.Start(func(src, msg string) {
			_ = itemsData.Append(src + "  →  " + msg)
			_ = statusB.Set("Received from " + src)
		})
		if err != nil {
			_ = statusB.Set("Receiver stopped: " + err.Error())
			log.Println(err)
		}
	}()

	w.ShowAndRun()
}
