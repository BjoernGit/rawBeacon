package netx

import (
	"errors"
	"net"
	"strings"
)

// -------------------- IP selection --------------------

// PickLocalIP returns a best-effort local IPv4:
// 1) RFC1918 private networks (10/8, 172.16/12, 192.168/16)
// 2) Link-local APIPA 169.254/16
// 3) "0.0.0.0" as last resort
func PickLocalIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "0.0.0.0"
	}

	var linkLocal string
	// heuristics to skip virtual/vpn/tunnel adapters by name
	skipNames := []string{"virtual", "vethernet", "vpn", "docker", "hyper-v", "vbox", "zerotier"}

	for _, iface := range ifaces {
		// must be up, not loopback
		if (iface.Flags&net.FlagUp) == 0 || (iface.Flags&net.FlagLoopback) != 0 {
			continue
		}
		name := strings.ToLower(iface.Name)
		skip := false
		for _, s := range skipNames {
			if strings.Contains(name, s) {
				skip = true
				break
			}
		}
		if skip {
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
			if isRFC1918(ip) {
				return ip.String()
			}
			if linkLocal == "" && isLinkLocal169(ip) {
				linkLocal = ip.String()
			}
		}
	}
	if linkLocal != "" {
		return linkLocal
	}
	return "0.0.0.0"
}

func isRFC1918(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	if ip4[0] == 10 {
		return true
	}
	if ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31 {
		return true
	}
	if ip4[0] == 192 && ip4[1] == 168 {
		return true
	}
	return false
}

func isLinkLocal169(ip net.IP) bool {
	ip4 := ip.To4()
	return ip4 != nil && ip4[0] == 169 && ip4[1] == 254
}

// -------------------- UDP listener --------------------

// ListenUDP binds an IPv4 UDP socket on the given port (":<port>") and returns *net.UDPConn.
func ListenUDP(port int) (*net.UDPConn, error) {
	lc := net.ListenConfig{}
	pc, err := lc.ListenPacket(nil, "udp4", ":"+itoa(port))
	if err != nil {
		return nil, err
	}
	udp, ok := pc.(*net.UDPConn)
	if !ok {
		_ = pc.Close()
		return nil, errors.New("not a UDP connection")
	}
	return udp, nil
}

// -------------------- Broadcaster --------------------

// Broadcaster writes datagrams to IPv4 broadcast and loopback on a fixed port.
// Typical usage:
//
//	bc, _ := netx.NewBroadcaster(47222)
//	defer bc.Close()
//	_ = bc.Send(payload)
type Broadcaster struct {
	port int
	bc   *net.UDPConn // 255.255.255.255:port
	lb   *net.UDPConn // 127.0.0.1:port
}

// NewBroadcaster dials broadcast and loopback destinations for the given port.
func NewBroadcaster(port int) (*Broadcaster, error) {
	b := &Broadcaster{port: port}

	bcastAddr, err := net.ResolveUDPAddr("udp4", "255.255.255.255:"+itoa(port))
	if err != nil {
		return nil, err
	}
	bc, err := net.DialUDP("udp4", nil, bcastAddr)
	if err != nil {
		return nil, err
	}
	b.bc = bc

	loopAddr, err := net.ResolveUDPAddr("udp4", "127.0.0.1:"+itoa(port))
	if err != nil {
		_ = b.Close()
		return nil, err
	}
	lb, err := net.DialUDP("udp4", nil, loopAddr)
	if err != nil {
		_ = b.Close()
		return nil, err
	}
	b.lb = lb
	return b, nil
}

// Send writes the same datagram to broadcast and loopback.
func (b *Broadcaster) Send(p []byte) error {
	if b.bc != nil {
		if _, err := b.bc.Write(p); err != nil {
			return err
		}
	}
	if b.lb != nil {
		if _, err := b.lb.Write(p); err != nil {
			return err
		}
	}
	return nil
}

// Close closes underlying UDP sockets.
func (b *Broadcaster) Close() error {
	var err1, err2 error
	if b.bc != nil {
		err1 = b.bc.Close()
	}
	if b.lb != nil {
		err2 = b.lb.Close()
	}
	if err1 != nil {
		return err1
	}
	return err2
}

// -------------------- small utils --------------------

// itoa avoids pulling strconv for a tiny helper.
func itoa(i int) string {
	// simple, sufficient for port numbers
	if i == 0 {
		return "0"
	}
	var buf [6]byte // ports fit into 5 chars, keep 6 for safety
	pos := len(buf)
	n := i
	for n > 0 {
		pos--
		buf[pos] = byte('0' + (n % 10))
		n /= 10
	}
	return string(buf[pos:])
}

// Itoa is an exported wrapper around the internal itoa helper.
// Kept to avoid importing strconv just for ports.
func Itoa(i int) string { return itoa(i) }
