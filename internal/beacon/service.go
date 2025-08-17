package beacon

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// Service encapsulates UDP listen/send state for manual or periodic beacons.
type Service struct {
	cfg     Config
	pc      net.PacketConn // UDP listener for inbound packets
	conn    *net.UDPConn   // UDP connection for outbound packets (broadcast)
	selfTag string         // Marker to filter out own packets
	host    string         // Cached local name for payload
}

// New prepares the UDP listener and broadcast connection.
// It does not start the receive loop yet.
func New(cfg Config) (*Service, error) {
	// Bind UDP listener on all interfaces and configured port.
	addr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	pc, err := net.ListenPacket("udp4", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}

	// Resolve broadcast destination and create a UDP connection for writes.
	raddr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", cfg.BroadcastAddr, cfg.Port))
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("resolve broadcast: %w", err)
	}
	conn, err := net.DialUDP("udp4", nil, raddr)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("dial broadcast: %w", err)
	}

	host := cfg.Name
	return &Service{
		cfg:     cfg,
		pc:      pc,
		conn:    conn,
		selfTag: "|" + host + "|",
		host:    host,
	}, nil
}

// Start runs a blocking receive loop.
// For each valid inbound beacon, handler(src, msg) is invoked.
// The loop ends on read error (e.g., when Close() is called).
func (s *Service) Start(handler func(src, msg string)) error {
	buf := make([]byte, 1500) // MTU-sized buffer
	for {
		n, src, err := s.pc.ReadFrom(buf)
		if err != nil {
			return err
		}
		msg := string(buf[:n])

		// Ignore packets that are clearly our own (simple heuristic).
		if strings.Contains(msg, s.selfTag) {
			continue
		}
		// Only accept our simple "protocol" prefix.
		if !strings.HasPrefix(msg, "rawbeacon|") {
			continue
		}

		if handler != nil {
			handler(src.String(), msg)
		}
	}
}

// SendOnce emits a single beacon payload immediately.
func (s *Service) SendOnce() error {
	payload := fmt.Sprintf("rawbeacon|%s|%d", s.host, time.Now().Unix())
	_, err := s.conn.Write([]byte(payload))
	return err
}

// Close releases sockets. Safe to call when stopping the service.
func (s *Service) Close() error {
	var err1, err2 error
	if s.conn != nil {
		err1 = s.conn.Close()
	}
	if s.pc != nil {
		err2 = s.pc.Close()
	}
	if err1 != nil {
		return err1
	}
	return err2
}
