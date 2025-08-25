package proto

import (
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/hypebeast/go-osc/osc"
)

const (
	// OSC addresses (wire contracts)
	AddrID           = "/beacon/id"            // b uid16, s tag, s ip
	AddrIDsRequest   = "/beacon/ids/request"   // s reply_ip, i reply_port, s request_id
	AddrIDsResponse  = "/beacon/ids/response"  // s request_id, i count, (b uid16, s tag, s ip)...
	AddrTagsRequest  = "/beacon/tags/request"  // s reply_ip, i reply_port, s request_id, s target_uid_hex
	AddrTagsResponse = "/beacon/tags/response" // b uid16, s primaryTag, s ip, i port, s request_id, i n, s...tags
)

// ---------- Helpers ----------

// UIDBytesToHex converts a 16-byte UID to hex.
func UIDBytesToHex(b []byte) string { return hex.EncodeToString(b) }

// UIDHexToBytes parses a hex UID (32 chars) back to 16 bytes.
func UIDHexToBytes(h string) ([]byte, error) {
	b, err := hex.DecodeString(h)
	if err != nil {
		return nil, err
	}
	if len(b) != 16 {
		return nil, fmt.Errorf("uid must be 16 bytes, got %d", len(b))
	}
	return b, nil
}

// ---------- /beacon/id ----------

func BuildID(uid []byte, name string, ip string, port int) *osc.Message {
	m := osc.NewMessage("/beacon/id")
	m.Append(uid)
	m.Append(name) // korrekt: Primärname/ID
	m.Append(ip)
	m.Append(int32(port))
	return m
}

func ParseID(msg *osc.Message) (uid16 []byte, name, ip string, port int, err error) {
	if msg.Address != AddrID || len(msg.Arguments) < 3 {
		return nil, "", "", 0, errors.New("invalid /beacon/id")
	}
	var ok bool
	if uid16, ok = msg.Arguments[0].([]byte); !ok || len(uid16) != 16 {
		return nil, "", "", 0, errors.New("id: uid must be 16 bytes")
	}
	name, _ = msg.Arguments[1].(string)
	ip, _ = msg.Arguments[2].(string)
	if len(msg.Arguments) >= 4 {
		if p, ok := msg.Arguments[3].(int32); ok {
			port = int(p)
		}
	}
	return
}

// ---------- /beacon/ids ----------

func BuildIDsRequest(replyIP string, replyPort int, reqID string) *osc.Message {
	m := osc.NewMessage(AddrIDsRequest)
	m.Append(replyIP)          // s
	m.Append(int32(replyPort)) // i
	m.Append(reqID)            // s
	return m
}

// peers: triplets of (uid16, tag, ip)
func BuildIDsResponse(reqID string, peers [][]interface{}) *osc.Message {
	m := osc.NewMessage(AddrIDsResponse)
	m.Append(reqID)
	m.Append(int32(len(peers)))
	for _, p := range peers {
		// expect: [ []byte uid16, string tag, string ip, int32 port ]
		m.Append(p[0])
		m.Append(p[1])
		m.Append(p[2])
		m.Append(p[3])
	}
	return m
}

type PeerTriplet struct {
	UID16 []byte
	Tag   string
	IP    string
	Port  int // NEW
}

func ParseIDsResponse(msg *osc.Message) (reqID string, peers []PeerTriplet, err error) {
	if msg.Address != AddrIDsResponse || len(msg.Arguments) < 2 {
		return "", nil, errors.New("invalid /beacon/ids/response")
	}
	reqID, _ = msg.Arguments[0].(string)
	cnt, _ := msg.Arguments[1].(int32)

	expect := 2 + int(cnt)*4
	if len(msg.Arguments) < expect {
		return "", nil, errors.New("ids/response: not enough args")
	}

	peers = make([]PeerTriplet, 0, cnt)
	for i := 0; i < int(cnt); i++ {
		base := 2 + i*4
		b, _ := msg.Arguments[base+0].([]byte)
		t, _ := msg.Arguments[base+1].(string)
		ip, _ := msg.Arguments[base+2].(string)
		port := 0
		if v, ok := msg.Arguments[base+3].(int32); ok {
			port = int(v)
		}
		peers = append(peers, PeerTriplet{UID16: b, Tag: t, IP: ip, Port: port})
	}
	return
}

// ---------- /beacon/tags ----------

func BuildTagsRequest(replyIP string, replyPort int, reqID, targetUIDHex string) *osc.Message {
	m := osc.NewMessage(AddrTagsRequest)
	m.Append(replyIP)          // s
	m.Append(int32(replyPort)) // i
	m.Append(reqID)            // s
	m.Append(targetUIDHex)     // s
	return m
}

func BuildTagsResponse(uid16 []byte, primaryTag, ip string, port int, reqID string, tags []string) *osc.Message {
	m := osc.NewMessage(AddrTagsResponse)
	m.Append(uid16)            // b
	m.Append(primaryTag)       // s
	m.Append(ip)               // s
	m.Append(int32(port))      // i
	m.Append(reqID)            // s
	m.Append(int32(len(tags))) // i
	for _, t := range tags {
		m.Append(t) // s
	}
	return m
}

type TagsPayload struct {
	UID16      []byte
	PrimaryTag string
	IP         string
	Port       int
	ReqID      string
	Tags       []string
}

func ParseTagsResponse(msg *osc.Message) (TagsPayload, error) {
	var out TagsPayload
	if msg.Address != AddrTagsResponse || len(msg.Arguments) < 6 {
		return out, errors.New("invalid /beacon/tags/response")
	}
	var ok bool
	if out.UID16, ok = msg.Arguments[0].([]byte); !ok || len(out.UID16) != 16 {
		return out, errors.New("tags/resp: uid invalid")
	}
	out.PrimaryTag, _ = msg.Arguments[1].(string)
	out.IP, _ = msg.Arguments[2].(string)
	if p, ok := msg.Arguments[3].(int32); ok {
		out.Port = int(p)
	}
	out.ReqID, _ = msg.Arguments[4].(string)
	n, _ := msg.Arguments[5].(int32)

	out.Tags = make([]string, 0, n)
	for i := 0; i < int(n) && 6+i < len(msg.Arguments); i++ {
		if s, ok := msg.Arguments[6+i].(string); ok {
			out.Tags = append(out.Tags, s)
		}
	}
	return out, nil
}
