package beacon

import (
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hypebeast/go-osc/osc"
	"github.com/vmihailenco/msgpack/v5"
)

// Message paths following OSC conventions
const (
	PathHello      = "/beacon/hello"
	PathQuery      = "/beacon/query"
	PathResponse   = "/beacon/response"
	PathPeerList   = "/beacon/peerlist"
	PathGoodbye    = "/beacon/goodbye"
	PathStats      = "/beacon/stats"
	PathInterfaces = "/beacon/interfaces"
)

type Role string

const (
	RoleMotive      Role = "motive"
	RoleUnityServer Role = "unity-server"
	RoleUnityClient Role = "unity-client"
	RoleESP32       Role = "esp32"
	RoleCreative    Role = "creative" // TouchDesigner, Max/MSP etc
)

type Peer struct {
	Tag       string
	Role      Role
	Endpoints []Endpoint
	LastSeen  time.Time
	TTL       time.Duration
}

type Endpoint struct {
	Interface string
	IP        net.IP
	Port      int
	Priority  int // 0=highest (ethernet), 1=wifi-primary, 2=wifi-secondary
}

type BeaconOSC struct {
	tag  string
	role Role
	port int

	interfaces map[string]*InterfaceHandler
	peers      map[string]*Peer
	mu         sync.RWMutex

	// Callbacks for Unity/Application integration
	OnPeerDiscovered func(peer *Peer)
	OnPeerLost       func(tag string)
	OnPeerUpdated    func(peer *Peer)

	stopCh chan struct{}
}

type InterfaceHandler struct {
	name     string
	ip       net.IP
	conn     *net.UDPConn
	client   *osc.Client
	server   *osc.Server
	priority int
}

func NewBeaconOSC(tag string, role Role, port int) (*BeaconOSC, error) {
	b := &BeaconOSC{
		tag:        tag,
		role:       role,
		port:       port,
		interfaces: make(map[string]*InterfaceHandler),
		peers:      make(map[string]*Peer),
		stopCh:     make(chan struct{}),
	}

	if err := b.initInterfaces(); err != nil {
		return nil, err
	}

	return b, nil
}

func (b *BeaconOSC) initInterfaces() error {
	ifaces, err := net.Interfaces()
	if err != nil {
		return err
	}

	for _, iface := range ifaces {
		// Skip loopback and down interfaces
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP.To4() == nil {
				continue
			}

			priority := b.getInterfacePriority(iface.Name)
			handler, err := b.createInterfaceHandler(iface.Name, ipNet.IP, priority)
			if err != nil {
				log.Printf("Failed to create handler for %s: %v", iface.Name, err)
				continue
			}

			b.interfaces[iface.Name] = handler
			go b.receiveLoop(handler)

			log.Printf("Initialized interface %s (%s) with priority %d",
				iface.Name, ipNet.IP, priority)
		}
	}

	if len(b.interfaces) == 0 {
		return fmt.Errorf("no usable network interfaces found")
	}

	return nil
}

func (b *BeaconOSC) getInterfacePriority(name string) int {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "eth"):
		return 0 // Ethernet highest priority
	case strings.Contains(lower, "wifi") || strings.Contains(lower, "wlan"):
		if strings.Contains(lower, "5g") {
			return 1 // 5GHz WiFi
		}
		return 2 // 2.4GHz WiFi
	default:
		return 3 // Other
	}
}

func (b *BeaconOSC) createInterfaceHandler(name string, ip net.IP, priority int) (*InterfaceHandler, error) {
	// Multicast address for discovery
	multicastAddr := "239.18.7.42"

	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", multicastAddr, b.port))
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenMulticastUDP("udp4", nil, addr)
	if err != nil {
		// Fallback to broadcast
		addr, err = net.ResolveUDPAddr("udp4", fmt.Sprintf(":%d", b.port))
		if err != nil {
			return nil, err
		}
		conn, err = net.ListenUDP("udp4", addr)
		if err != nil {
			return nil, err
		}
	}

	// Enable broadcast
	conn.SetReadBuffer(65536)

	client := osc.NewClient(multicastAddr, b.port)

	return &InterfaceHandler{
		name:     name,
		ip:       ip,
		conn:     conn,
		client:   client,
		priority: priority,
	}, nil
}

func (b *BeaconOSC) Start() {
	// Start hello beacon
	go b.helloLoop()

	// Start peer cleanup
	go b.cleanupLoop()

	// Start local API server for Unity
	go b.startLocalAPI()
}

func (b *BeaconOSC) helloLoop() {
	ticker := time.NewTicker(b.getHelloInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			b.sendHello()
			ticker.Reset(b.getHelloInterval())
		case <-b.stopCh:
			b.sendGoodbye()
			return
		}
	}
}

func (b *BeaconOSC) getHelloInterval() time.Duration {
	b.mu.RLock()
	peerCount := len(b.peers)
	b.mu.RUnlock()

	// Adaptive backoff based on peer count
	switch {
	case peerCount == 0:
		return 1 * time.Second // Fast discovery
	case peerCount < 3:
		return 5 * time.Second // Few peers
	default:
		return 30 * time.Second // Stable network
	}
}

func (b *BeaconOSC) sendHello() {
	msg := osc.NewMessage(PathHello)
	msg.Append(b.tag)
	msg.Append(string(b.role))
	msg.Append(int32(time.Now().Unix()))

	// Send on all interfaces
	for _, handler := range b.interfaces {
		// Append interface-specific data
		enrichedMsg := osc.NewMessage(PathHello)
		enrichedMsg.Append(b.tag)
		enrichedMsg.Append(string(b.role))
		enrichedMsg.Append(handler.name)
		enrichedMsg.Append(handler.ip.String())
		enrichedMsg.Append(int32(b.port))
		enrichedMsg.Append(int32(handler.priority))
		enrichedMsg.Append(int32(time.Now().Unix()))

		if err := enrichedMsg.Write(handler.conn); err != nil {
			log.Printf("Failed to send hello on %s: %v", handler.name, err)
		}
	}
}

func (b *BeaconOSC) sendGoodbye() {
	msg := osc.NewMessage(PathGoodbye)
	msg.Append(b.tag)
	msg.Append(int32(time.Now().Unix()))

	for _, handler := range b.interfaces {
		msg.Write(handler.conn)
	}
}

func (b *BeaconOSC) receiveLoop(handler *InterfaceHandler) {
	buf := make([]byte, 65536)

	for {
		select {
		case <-b.stopCh:
			return
		default:
		}

		handler.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, addr, err := handler.conn.ReadFromUDP(buf)
		if err != nil {
			if !isTimeout(err) {
				log.Printf("Read error on %s: %v", handler.name, err)
			}
			continue
		}

		packet, err := osc.ParsePacket(string(buf[:n]))
		if err != nil {
			continue
		}

		b.handlePacket(packet, addr, handler)
	}
}

func (b *BeaconOSC) handlePacket(packet osc.Packet, from *net.UDPAddr, handler *InterfaceHandler) {
	switch p := packet.(type) {
	case *osc.Message:
		b.handleMessage(p, from, handler)
	case *osc.Bundle:
		for _, msg := range p.Messages {
			b.handleMessage(msg, from, handler)
		}
	}
}

func (b *BeaconOSC) handleMessage(msg *osc.Message, from *net.UDPAddr, handler *InterfaceHandler) {
	switch msg.Address {
	case PathHello:
		b.handleHello(msg, from, handler)
	case PathQuery:
		b.handleQuery(msg, from, handler)
	case PathResponse:
		b.handleResponse(msg, from, handler)
	case PathPeerList:
		b.handlePeerList(msg, from, handler)
	case PathGoodbye:
		b.handleGoodbye(msg, from, handler)
	default:
		// Allow creative tools to use custom paths
		if strings.HasPrefix(msg.Address, "/beacon/") {
			log.Printf("Custom OSC message: %s from %s", msg.Address, from)
		}
	}
}

func (b *BeaconOSC) handleHello(msg *osc.Message, from *net.UDPAddr, handler *InterfaceHandler) {
	if len(msg.Arguments) < 7 {
		return
	}

	tag, _ := msg.Arguments[0].(string)
	role, _ := msg.Arguments[1].(string)
	ifaceName, _ := msg.Arguments[2].(string)
	ip, _ := msg.Arguments[3].(string)
	port, _ := msg.Arguments[4].(int32)
	priority, _ := msg.Arguments[5].(int32)
	// timestamp at index 6

	// Ignore our own messages
	if tag == b.tag {
		return
	}

	endpoint := Endpoint{
		Interface: ifaceName,
		IP:        net.ParseIP(ip),
		Port:      int(port),
		Priority:  int(priority),
	}

	b.mu.Lock()
	peer, exists := b.peers[tag]
	if !exists {
		peer = &Peer{
			Tag:      tag,
			Role:     Role(role),
			LastSeen: time.Now(),
			TTL:      60 * time.Second,
		}
		b.peers[tag] = peer

		if b.OnPeerDiscovered != nil {
			go b.OnPeerDiscovered(peer)
		}
	}

	// Update or add endpoint
	peer.LastSeen = time.Now()
	endpointExists := false
	for i, ep := range peer.Endpoints {
		if ep.Interface == ifaceName {
			peer.Endpoints[i] = endpoint
			endpointExists = true
			break
		}
	}
	if !endpointExists {
		peer.Endpoints = append(peer.Endpoints, endpoint)
	}
	b.mu.Unlock()

	if exists && b.OnPeerUpdated != nil {
		go b.OnPeerUpdated(peer)
	}
}

func (b *BeaconOSC) handleGoodbye(msg *osc.Message, from *net.UDPAddr, handler *InterfaceHandler) {
	if len(msg.Arguments) < 1 {
		return
	}

	tag, _ := msg.Arguments[0].(string)

	b.mu.Lock()
	if _, exists := b.peers[tag]; exists {
		delete(b.peers, tag)
		b.mu.Unlock()

		if b.OnPeerLost != nil {
			go b.OnPeerLost(tag)
		}
	} else {
		b.mu.Unlock()
	}
}

func (b *BeaconOSC) handlePeerList(msg *osc.Message, from *net.UDPAddr, handler *InterfaceHandler) {
	// Peer list is sent as MessagePack blob for efficiency
	if len(msg.Arguments) < 1 {
		return
	}

	data, ok := msg.Arguments[0].([]byte)
	if !ok {
		return
	}

	var peerList []Peer
	if err := msgpack.Unmarshal(data, &peerList); err != nil {
		log.Printf("Failed to unmarshal peer list: %v", err)
		return
	}

	// Merge peer list (gossip protocol)
	b.mu.Lock()
	for _, peer := range peerList {
		if peer.Tag == b.tag {
			continue
		}

		existing, exists := b.peers[peer.Tag]
		if !exists || existing.LastSeen.Before(peer.LastSeen) {
			b.peers[peer.Tag] = &peer
		}
	}
	b.mu.Unlock()
}

func (b *BeaconOSC) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			b.cleanupPeers()
		case <-b.stopCh:
			return
		}
	}
}

func (b *BeaconOSC) cleanupPeers() {
	b.mu.Lock()
	now := time.Now()

	for tag, peer := range b.peers {
		if now.Sub(peer.LastSeen) > peer.TTL {
			delete(b.peers, tag)

			if b.OnPeerLost != nil {
				go b.OnPeerLost(tag)
			}
		}
	}
	b.mu.Unlock()
}

// Local API for Unity integration (simplified)
func (b *BeaconOSC) startLocalAPI() {
	addr, err := net.ResolveUDPAddr("udp4", "127.0.0.1:45222")
	if err != nil {
		log.Printf("Failed to start local API: %v", err)
		return
	}

	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		log.Printf("Failed to start local API: %v", err)
		return
	}
	defer conn.Close()

	buf := make([]byte, 4096)
	for {
		select {
		case <-b.stopCh:
			return
		default:
		}

		conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, clientAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if !isTimeout(err) {
				log.Printf("Local API read error: %v", err)
			}
			continue
		}

		// Simple text protocol for Unity
		// Commands: "GET_PEERS", "RESOLVE:tag", "GET_BEST:tag"
		cmd := string(buf[:n])
		response := b.handleLocalCommand(cmd)
		conn.WriteToUDP([]byte(response), clientAddr)
	}
}

func (b *BeaconOSC) handleLocalCommand(cmd string) string {
	parts := strings.Split(cmd, ":")

	switch parts[0] {
	case "GET_PEERS":
		b.mu.RLock()
		defer b.mu.RUnlock()

		var result []string
		for tag, peer := range b.peers {
			best := b.getBestEndpoint(peer)
			if best != nil {
				result = append(result, fmt.Sprintf("%s|%s|%s|%d",
					tag, peer.Role, best.IP, best.Port))
			}
		}
		return strings.Join(result, "\n")

	case "RESOLVE":
		if len(parts) < 2 {
			return "ERROR:Missing tag"
		}

		b.mu.RLock()
		peer, exists := b.peers[parts[1]]
		b.mu.RUnlock()

		if !exists {
			return "ERROR:Not found"
		}

		best := b.getBestEndpoint(peer)
		if best == nil {
			return "ERROR:No endpoint"
		}

		return fmt.Sprintf("%s|%d", best.IP, best.Port)

	default:
		return "ERROR:Unknown command"
	}
}

func (b *BeaconOSC) getBestEndpoint(peer *Peer) *Endpoint {
	if len(peer.Endpoints) == 0 {
		return nil
	}

	// Return endpoint with lowest priority number (0 = best)
	best := peer.Endpoints[0]
	for _, ep := range peer.Endpoints {
		if ep.Priority < best.Priority {
			best = ep
		}
	}

	return &best
}

func (b *BeaconOSC) Stop() {
	close(b.stopCh)

	// Close all connections
	for _, handler := range b.interfaces {
		handler.conn.Close()
	}
}

func isTimeout(err error) bool {
	netErr, ok := err.(net.Error)
	return ok && netErr.Timeout()
}

// Unity-friendly methods
func (b *BeaconOSC) GetPeerByTag(tag string) *Peer {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.peers[tag]
}

func (b *BeaconOSC) GetPeersByRole(role Role) []*Peer {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var result []*Peer
	for _, peer := range b.peers {
		if peer.Role == role {
			result = append(result, peer)
		}
	}
	return result
}

func (b *BeaconOSC) ResolveTag(tag string) (string, int, error) {
	peer := b.GetPeerByTag(tag)
	if peer == nil {
		return "", 0, fmt.Errorf("tag not found: %s", tag)
	}

	best := b.getBestEndpoint(peer)
	if best == nil {
		return "", 0, fmt.Errorf("no endpoint for tag: %s", tag)
	}

	return best.IP.String(), best.Port, nil
}
