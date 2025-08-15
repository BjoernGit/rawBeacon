package main

import (
	"log"
	"os"

	"github.com/BjoernGit/rawBeacon/internal/beacon"
)

func main() {
	name, _ := os.Hostname()
	if name == "" {
		name = "unknown"
	}

	cfg := beacon.Config{
		Port: 45221,
		// 255.255.255.255 = Broadcast an alle im Subnetz
		BroadcastAddr: "255.255.255.255",
		Name:          name,
		Interval:      2000, // ms
	}
	log.Printf("rawBeacon starting as %q on port %d ...\n", cfg.Name, cfg.Port)
	if err := beacon.Run(cfg); err != nil {
		log.Fatal(err)
	}
}
