package beacon

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hypebeast/go-osc/osc"
)

// ServiceOSC wraps the enhanced OSC beacon for GUI integration
// This replaces the old Service struct when using OSC mode
type ServiceOSC struct {
	beacon    *BeaconOSC
	localConn *net.UDPConn // For manual SendOnce() broadcasts
	mu        sync.Mutex
}

// NewOSC creates a new OSC-based service that's compatible with the GUI
// This is what main.go should call instead of beacon.New()
func NewOSC(cfg Config) (*ServiceOSC, error) {
	// Determine role based on config or hostname
	role := RoleUnityServer // Default
	if strings.Contains(strings.ToLower(cfg.Name), "motive") {
		role = RoleMotive
	} else if strings.Contains(strings.ToLower(cfg.Name), "esp") {
		role = RoleESP32
	}

	// Create the enhanced OSC beacon
	beacon, err := NewBeaconOSC(cfg.Name, role, cfg.Port)
	if err != nil {
		return nil, fmt.Errorf("failed to create OSC beacon: %w", err)
	}

	// Create a simple UDP connection for SendOnce()
	broadcastAddr := fmt.Sprintf("%s:%d", cfg.BroadcastAddr, cfg.Port)
	raddr, err := net.ResolveUDPAddr("udp4", broadcastAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve broadcast: %w", err)
	}

	localConn, err := net.DialUDP("udp4", nil, raddr)
	if err != nil {
		return nil, fmt.Errorf("dial broadcast: %w", err)
	}

	return &ServiceOSC{
		beacon:    beacon,
		localConn: localConn,
	}, nil
}

// Start begins the beacon receive loop and discovery process
// handler is called for each received message (compatible with GUI)
func (s *ServiceOSC) Start(handler func(src, msg string)) error {
	// Set up callbacks that convert to the simple handler format
	if handler != nil {
		s.beacon.OnPeerDiscovered = func(peer *Peer) {
			msg := fmt.Sprintf("DISCOVERED|%s|%s", peer.Tag, peer.Role)
			handler(peer.Endpoints[0].IP.String(), msg)
		}

		s.beacon.OnPeerUpdated = func(peer *Peer) {
			msg := fmt.Sprintf("UPDATED|%s|%s", peer.Tag, peer.Role)
			if len(peer.Endpoints) > 0 {
				handler(peer.Endpoints[0].IP.String(), msg)
			}
		}

		s.beacon.OnPeerLost = func(tag string) {
			handler("system", fmt.Sprintf("LOST|%s", tag))
		}
	}

	// Start the beacon
	s.beacon.Start()

	// Keep the Start method blocking as expected by main.go
	<-s.beacon.stopCh
	return nil
}

// SendOnce sends a single beacon broadcast (for "Ping now" button)
func (s *ServiceOSC) SendOnce() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Create and send an OSC hello message
	msg := osc.NewMessage("/beacon/hello")
	msg.Append(s.beacon.tag)
	msg.Append(string(s.beacon.role))

	// Get primary interface info
	var primaryIP string
	var primaryInterface string
	for name, handler := range s.beacon.interfaces {
		if handler.priority == 0 || primaryIP == "" {
			primaryIP = handler.ip.String()
			primaryInterface = name
		}
	}

	msg.Append(primaryInterface)
	msg.Append(primaryIP)
	msg.Append(int32(s.beacon.port))
	msg.Append(int32(0)) // priority
	msg.Append(int32(time.Now().Unix()))

	data, err := msg.MarshalBinary()
	if err != nil {
		return fmt.Errorf("marshal OSC message: %w", err)
	}

	_, err = s.localConn.Write(data)
	return err
}

// Close stops the beacon and releases resources
func (s *ServiceOSC) Close() error {
	s.beacon.Stop()

	if s.localConn != nil {
		return s.localConn.Close()
	}
	return nil
}

// Additional methods for GUI integration

// GetPeers returns current discovered peers in GUI-friendly format
func (s *ServiceOSC) GetPeers() []string {
	peers := s.beacon.GetPeersByRole("") // Empty string = all roles

	var result []string
	for _, peer := range peers {
		if len(peer.Endpoints) > 0 {
			ep := peer.Endpoints[0]
			result = append(result, fmt.Sprintf("%s (%s) @ %s:%d",
				peer.Tag, peer.Role, ep.IP, ep.Port))
		}
	}
	return result
}

// GetStatus returns current beacon status
func (s *ServiceOSC) GetStatus() string {
	peerCount := len(s.beacon.peers)
	interfaceCount := len(s.beacon.interfaces)

	return fmt.Sprintf("Interfaces: %d, Peers: %d", interfaceCount, peerCount)
}
