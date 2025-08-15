package beacon

import (
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

type Config struct {
	Port          int
	BroadcastAddr string
	Name          string
	Interval      int
}

func Run(cfg Config) error {
	addr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	pc, err := net.ListenPacket("udp4", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	defer pc.Close()

	raddr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", cfg.BroadcastAddr, cfg.Port))
	if err != nil {
		return fmt.Errorf("resolve broadcast: %w", err)
	}
	conn, err := net.DialUDP("udp4", nil, raddr)
	if err != nil {
		return fmt.Errorf("dial broadcast: %w", err)
	}
	defer conn.Close()

	host := cfg.Name
	selfTag := "|" + host + "|"

	go func() {
		t := time.NewTicker(time.Duration(cfg.Interval) * time.Millisecond)
		defer t.Stop()
		for range t.C {
			msg := fmt.Sprintf("rawbeacon|%s|%d", host, time.Now().Unix())
			_, _ = conn.Write([]byte(msg))
		}
	}()

	buf := make([]byte, 1500)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			return err
		}
		msg := string(buf[:n])
		if strings.Contains(msg, selfTag) {
			continue
		}
		if !strings.HasPrefix(msg, "rawbeacon|") {
			continue
		}
		fmt.Printf("[DISCOVERED] %s -> %s\n", src.String(), msg)
	}
}

func init() {
	if isWindows() {
		log.Println("Hinweis: Erlaube die App in der Windows-Firewall (Private Netzwerke), falls gefragt.")
	}
}
func isWindows() bool { return strings.Contains(strings.ToLower(os.Getenv("OS")), "windows") }
