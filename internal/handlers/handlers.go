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
		uid16, tag, ip, err := proto.ParseID(msg)
		if err != nil {
			return
		}
		uidHex := proto.UIDBytesToHex(uid16)
		// ignore our own UID
		if uidHex == proto.UIDBytesToHex(localUID) {
			return
		}
		peerStore.Upsert(store.Peer{
			UID:      uidHex,
			Tag:      tag,
			IP:       ip,
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
			triplets = append(triplets, []interface{}{uidB, p.Tag, p.IP})
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

		// Only respond if targetUID empty or matches us
		if targetUID != "" && !strings.EqualFold(targetUID, proto.UIDBytesToHex(localUID)) {
			return
		}

		tags := tagStore.List()
		resp := proto.BuildTagsResponse(localUID, localTag, netx.PickLocalIP(), recvPort, reqID, tags)

		addr := net.JoinHostPort(replyIP, netx.Itoa(replyPort))
		ua, err := net.ResolveUDPAddr("udp4", addr)
		if err != nil {
			log.Println("tags/response resolve:", err)
			return
		}
		conn, err := net.DialUDP("udp4", nil, ua)
		if err != nil {
			log.Println("tags/response dial:", err)
			return
		}
		defer conn.Close()

		data, _ := resp.MarshalBinary()
		_, _ = conn.Write(data)
	})
}
