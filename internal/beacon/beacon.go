package beacon

import (
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/hypebeast/go-osc/osc"
)

// Config keeps runtime settings for the beacon.
type Config struct {
	Port          int    // UDP destination port to listen/send on
	BroadcastAddr string // e.g. "255.255.255.255"
	Name          string // local hostname to advertise (fallback to os.Hostname)
	Interval      int    // ms; used only in periodic mode (not required here)
}

// Run starts an OSC-based beacon: it listens on cfg.Port and
// periodically (every cfg.Interval ms) broadcasts an OSC message:
//
//	Address: /rawbeacon
//	Args:    hostname (string), unix timestamp (int32)
func Run(cfg Config) error {
	// Bind UDP listener on all interfaces.
	addr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	pc, err := net.ListenPacket("udp4", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	defer pc.Close()

	// Resolve broadcast target and open UDP connection for writes.
	raddr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", cfg.BroadcastAddr, cfg.Port))
	if err != nil {
		return fmt.Errorf("resolve broadcast: %w", err)
	}
	conn, err := net.DialUDP("udp4", nil, raddr)
	if err != nil {
		return fmt.Errorf("dial broadcast: %w", err)
	}
	defer conn.Close()

	// Discover local host name (fallback).
	host := cfg.Name
	if host == "" {
		if h, _ := os.Hostname(); h != "" {
			host = h
		} else {
			host = "unknown"
		}
	}

	// Sender (optional periodic demo). If you trigger manually via GUI,
	// you can remove this goroutine and call sendOSC once on button press.
	if cfg.Interval > 0 {
		go func() {
			t := time.NewTicker(time.Duration(cfg.Interval) * time.Millisecond)
			defer t.Stop()
			for range t.C {
				if err := sendOSC(conn, host); err != nil {
					log.Println("send error:", err)
				}
			}
		}()
	}

	// Receiver loop: parse incoming datagrams as OSC and print discoveries.
	buf := make([]byte, 1500)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			return err
		}

		// go-osc expects string input for parsing.
		pkt, err := osc.ParsePacket(string(buf[:n]))
		if err != nil {
			continue // ignore non-OSC packets
		}

		switch m := pkt.(type) {
		case *osc.Message:
			// Expect /rawbeacon <string hostname> <int32 timestamp>
			if m.Address != "/rawbeacon" || len(m.Arguments) < 2 {
				continue
			}
			hn, _ := m.Arguments[0].(string)

			// Ignore our own broadcasts.
			if strings.EqualFold(hn, host) {
				continue
			}

			var ts int64
			if v, ok := m.Arguments[1].(int32); ok {
				ts = int64(v)
			}

			fmt.Printf("[DISCOVERED] %s -> /rawbeacon host=%s ts=%d\n",
				src.String(), hn, ts)
		}
	}
}

// sendOSC builds and writes one OSC message using the provided UDP conn.
func sendOSC(conn *net.UDPConn, host string) error {
	msg := osc.NewMessage("/rawbeacon")
	msg.Append(host)                     // string
	msg.Append(int32(time.Now().Unix())) // OSC int is 32-bit

	blob, err := msg.MarshalBinary()
	if err != nil {
		return err
	}
	_, err = conn.Write(blob)
	return err
}

func init() {
	if isWindows() {
		log.Println("Hinweis: Erlaube die App in der Windows-Firewall (Privates Netzwerk), falls gefragt.")
	}
}
func isWindows() bool { return strings.Contains(strings.ToLower(os.Getenv("OS")), "windows") }
