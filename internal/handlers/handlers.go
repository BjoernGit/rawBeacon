package handlers

import (
	"log"
	"net"
	"strings"
	"time"

	"github.com/hypebeast/go-osc/osc"

	"github.com/BjoernGit/rawBeacon/internal/netx"
	"github.com/BjoernGit/rawBeacon/internal/proto"
	"github.com/BjoernGit/rawBeacon/internal/store"
)

// Register installs all handlers into a dispatcher.
// You pass in your stores + local identity.
func Register(
	disp *osc.StandardDispatcher,
	peerStore *store.PeerStore,
	tagStore *store.TagStore,
	localUID []byte,
	localTag string,
	recvPort int,
) {
	// ---------------- /beacon/id ----------------
	_ = disp.AddMsgHandler(proto.AddrID, func(msg *osc.Message) {
		uid16, tag, ip, port, err := proto.ParseID(msg)
		if err != nil {
			return
		}
		uidHex := proto.UIDBytesToHex(uid16)
		if uidHex == proto.UIDBytesToHex(localUID) {
			return
		}
		peerStore.Upsert(store.Peer{
			UID:      uidHex,
			Tag:      tag,
			IP:       ip,
			RecvPort: port, // <— NEW
			LastSeen: time.Now(),
		})
	})

	// ---------------- /beacon/ids/request ----------------
	_ = disp.AddMsgHandler(proto.AddrIDsRequest, func(msg *osc.Message) {
		if len(msg.Arguments) < 3 {
			log.Println("ids/request: not enough args")
			return
		}
		replyIP, _ := msg.Arguments[0].(string)
		replyPort32, _ := msg.Arguments[1].(int32)
		reqID, _ := msg.Arguments[2].(string)
		replyPort := int(replyPort32)

		// snapshot of known peers
		snap := peerStore.Snapshot()
		triplets := make([][]interface{}, 0, len(snap))
		for _, p := range snap {
			uidB, _ := proto.UIDHexToBytes(p.UID)
			triplets = append(triplets, []interface{}{uidB, p.Tag, p.IP, int32(p.RecvPort)})
		}
		resp := proto.BuildIDsResponse(reqID, triplets)

		addr := net.JoinHostPort(replyIP, netx.Itoa(replyPort))
		ua, err := net.ResolveUDPAddr("udp4", addr)
		if err != nil {
			log.Println("ids/response resolve:", err)
			return
		}
		conn, err := net.DialUDP("udp4", nil, ua)
		if err != nil {
			log.Println("ids/response dial:", err)
			return
		}
		defer conn.Close()

		data, _ := resp.MarshalBinary()
		_, _ = conn.Write(data)
	})

	// ---------------- /beacon/tags/request ----------------
	_ = disp.AddMsgHandler(proto.AddrTagsRequest, func(msg *osc.Message) {
		if len(msg.Arguments) < 4 {
			log.Println("tags/request: not enough args")
			return
		}
		replyIP, _ := msg.Arguments[0].(string)
		replyPort32, _ := msg.Arguments[1].(int32)
		reqID, _ := msg.Arguments[2].(string)
		targetUID, _ := msg.Arguments[3].(string)
		replyPort := int(replyPort32)

		myUIDHex := proto.UIDBytesToHex(localUID)

		// Case 1: empty or me -> answer with my tags
		if targetUID == "" || strings.EqualFold(targetUID, myUIDHex) {
			tags := tagStore.List()
			resp := proto.BuildTagsResponse(localUID, localTag, netx.PickLocalIP(), recvPort, reqID, tags)

			addr := net.JoinHostPort(replyIP, netx.Itoa(replyPort))
			ua, err := net.ResolveUDPAddr("udp4", addr)
			if err != nil {
				log.Println("tags/resp resolve:", err)
				return
			}
			conn, err := net.DialUDP("udp4", nil, ua)
			if err != nil {
				log.Println("tags/resp dial:", err)
				return
			}
			defer conn.Close()

			data, _ := resp.MarshalBinary()
			_, _ = conn.Write(data)
			return
		}

		// Case 2: proxy to target beacon (forward the same request so target replies directly to consumer)
		// Lookup target peer
		var target *store.Peer
		for _, p := range peerStore.Snapshot() {
			if strings.EqualFold(p.UID, targetUID) {
				tmp := p
				target = &tmp
				break
			}
		}
		if target == nil || target.RecvPort == 0 || target.IP == "" {
			log.Println("tags/proxy: target not known or missing port/ip")
			return
		}

		// Build the same request with original reply_ip/port and reqID/target_uid
		fwd := proto.BuildTagsRequest(replyIP, replyPort, reqID, targetUID)

		dst := net.JoinHostPort(target.IP, netx.Itoa(target.RecvPort))
		ua, err := net.ResolveUDPAddr("udp4", dst)
		if err != nil {
			log.Println("tags/proxy resolve:", err)
			return
		}
		conn, err := net.DialUDP("udp4", nil, ua)
		if err != nil {
			log.Println("tags/proxy dial:", err)
			return
		}
		defer conn.Close()

		data, _ := fwd.MarshalBinary()
		_, _ = conn.Write(data)
	})

}
