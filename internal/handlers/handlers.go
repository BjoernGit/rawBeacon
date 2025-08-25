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

// Register wires all OSC message handlers to the provided dispatcher.
// name = primary human-readable ID of this beacon (what used to be "localTag").
// recvPort = this beacon's UDP receive port.
func Register(
	disp *osc.StandardDispatcher,
	peerStore *store.PeerStore,
	tagStore *store.TagStore,
	localUID []byte,
	localName string,
	recvPort int,
) {
	myUIDHex := proto.UIDBytesToHex(localUID)

	// -------- /beacon/id --------
	// b uid16, s name, s ip, i port (port optional for backward-compat)
	_ = disp.AddMsgHandler(proto.AddrID, func(msg *osc.Message) {
		uid16, name, ip, port, err := proto.ParseID(msg)
		if err != nil {
			log.Println("handlers: /beacon/id parse error:", err)
			return
		}
		uidHex := proto.UIDBytesToHex(uid16)
		if uidHex == myUIDHex {
			// ignore my own broadcast
			return
		}

		peerStore.Upsert(store.Peer{
			UID:      uidHex,
			Name:     name,
			IP:       ip,
			RecvPort: port,
			LastSeen: time.Now(),
		})
	})

	// -------- /beacon/ids/request --------
	// args: s reply_ip, i reply_port, s request_id
	_ = disp.AddMsgHandler(proto.AddrIDsRequest, func(msg *osc.Message) {
		replyIP, replyPort, reqID, err := proto.ParseIDsRequest(msg)
		if err != nil {
			log.Println("handlers: /beacon/ids/request parse error:", err)
			return
		}
		log.Printf("handlers: RECV ids/request reply=%s:%d reqID=%s", replyIP, replyPort, reqID)

		// Snapshot peers (exclude myself if present)
		snap := peerStore.Snapshot()
		triplets := make([][]interface{}, 0, len(snap))
		for _, p := range snap {
			if strings.EqualFold(p.UID, myUIDHex) {
				continue
			}
			uidB, _ := proto.UIDHexToBytes(p.UID)
			triplets = append(triplets, []interface{}{
				uidB,              // b uid16
				p.Name,            // s name
				p.IP,              // s ip
				int32(p.RecvPort), // i port (may be 0 if unknown)
			})
		}

		resp := proto.BuildIDsResponse(reqID, triplets)
		data, _ := resp.MarshalBinary()

		dst := net.JoinHostPort(replyIP, netx.Itoa(replyPort))
		ua, err := net.ResolveUDPAddr("udp4", dst)
		if err != nil {
			log.Println("handlers: ids/resp resolve:", err)
			return
		}
		conn, err := net.DialUDP("udp4", nil, ua)
		if err != nil {
			log.Println("handlers: ids/resp dial:", err)
			return
		}
		defer conn.Close()

		if _, err := conn.Write(data); err != nil {
			log.Println("handlers: ids/resp write:", err)
		} else {
			log.Printf("handlers: SEND ids/response -> %s count=%d", dst, len(triplets))
		}
	})

	// -------- /beacon/tags/request --------
	// args: s reply_ip, i reply_port, s request_id, s target_uid_hex[, i max_age_ms]
	_ = disp.AddMsgHandler(proto.AddrTagsRequest, func(msg *osc.Message) {
		replyIP, replyPort, reqID, targetUID, _, err := proto.ParseTagsRequest(msg)
		if err != nil {
			log.Println("handlers: /beacon/tags/request parse error:", err)
			return
		}
		log.Printf("handlers: RECV tags/request reply=%s:%d target=%s reqID=%s",
			replyIP, replyPort, targetUID, reqID)

		// Case 1: empty or me -> answer with *my* tags
		if targetUID == "" || strings.EqualFold(targetUID, myUIDHex) {
			tags := tagStore.List()
			resp := proto.BuildTagsResponse(localUID, localName, netx.PickLocalIP(), recvPort, reqID, tags)
			data, _ := resp.MarshalBinary()

			dst := net.JoinHostPort(replyIP, netx.Itoa(replyPort))
			ua, err := net.ResolveUDPAddr("udp4", dst)
			if err != nil {
				log.Println("handlers: tags/resp resolve:", err)
				return
			}
			conn, err := net.DialUDP("udp4", nil, ua)
			if err != nil {
				log.Println("handlers: tags/resp dial:", err)
				return
			}
			defer conn.Close()

			if _, err := conn.Write(data); err != nil {
				log.Println("handlers: tags/resp write:", err)
			} else {
				log.Printf("handlers: SEND tags/response (self) -> %s tags=%v", dst, tags)
			}
			return
		}

		// Case 2: proxy an Ziel; Ziel antwortet direkt an Consumer
		var target *store.Peer
		for _, p := range peerStore.Snapshot() {
			if strings.EqualFold(p.UID, targetUID) {
				tmp := p
				target = &tmp
				break
			}
		}
		if target == nil || target.IP == "" || target.RecvPort == 0 {
			log.Printf("handlers: tags/proxy: unknown target or missing ip/port (uid=%s)", targetUID)
			return
		}

		fwd := proto.BuildTagsRequest(replyIP, replyPort, reqID, targetUID)
		data, _ := fwd.MarshalBinary()

		dst := net.JoinHostPort(target.IP, netx.Itoa(target.RecvPort))
		ua, err := net.ResolveUDPAddr("udp4", dst)
		if err != nil {
			log.Println("handlers: tags/proxy resolve:", err)
			return
		}
		conn, err := net.DialUDP("udp4", nil, ua)
		if err != nil {
			log.Println("handlers: tags/proxy dial:", err)
			return
		}
		defer conn.Close()

		if _, err := conn.Write(data); err != nil {
			log.Println("handlers: tags/proxy write:", err)
		} else {
			log.Printf("handlers: FORWARD tags/request -> %s (target_uid=%s)", dst, targetUID)
		}
	})
}
