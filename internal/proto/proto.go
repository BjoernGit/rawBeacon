package proto

import (
	"encoding/hex"
	"fmt"

	"github.com/hypebeast/go-osc/osc"
)

// OSC-Adressen
const (
	AddrID           = "/beacon/id"
	AddrIDsRequest   = "/beacon/ids/request"
	AddrIDsResponse  = "/beacon/ids/response"
	AddrTagsRequest  = "/beacon/tags/request"
	AddrTagsResponse = "/beacon/tags/response"
)

// ---------- Helpers: UID <-> Hex ----------

func UIDBytesToHex(b []byte) string { return hex.EncodeToString(b) }

func UIDHexToBytes(s string) ([]byte, error) {
	dst := make([]byte, hex.DecodedLen(len(s)))
	n, err := hex.Decode(dst, []byte(s))
	if err != nil {
		return nil, err
	}
	return dst[:n], nil
}

// ---------- /beacon/id ----------

// BuildID: b uid16, s name, s ip, i port
func BuildID(uid []byte, name string, ip string, port int) *osc.Message {
	m := osc.NewMessage(AddrID)
	m.Append(uid)
	m.Append(name)
	m.Append(ip)
	m.Append(int32(port))
	return m
}

// ParseID liest b uid16, s name, s ip, [i port]
func ParseID(msg *osc.Message) (uid []byte, name, ip string, port int, err error) {
	if len(msg.Arguments) < 3 {
		return nil, "", "", 0, fmt.Errorf("ParseID: want >=3 args, got %d", len(msg.Arguments))
	}
	b, ok := msg.Arguments[0].([]byte)
	if !ok {
		return nil, "", "", 0, fmt.Errorf("ParseID: arg0 not []byte")
	}
	name, _ = msg.Arguments[1].(string)
	ip, _ = msg.Arguments[2].(string)
	port = 0
	if len(msg.Arguments) >= 4 {
		if p32, ok := msg.Arguments[3].(int32); ok {
			port = int(p32)
		} else if p64, ok := msg.Arguments[3].(int64); ok {
			port = int(p64)
		}
	}
	return b, name, ip, port, nil
}

// ---------- /beacon/ids ----------

// ParseIDsRequest: s reply_ip, i reply_port, s request_id
func ParseIDsRequest(msg *osc.Message) (replyIP string, replyPort int, reqID string, err error) {
	if len(msg.Arguments) < 3 {
		return "", 0, "", fmt.Errorf("ParseIDsRequest: want 3 args, got %d", len(msg.Arguments))
	}
	ip, ok := msg.Arguments[0].(string)
	if !ok {
		return "", 0, "", fmt.Errorf("ParseIDsRequest: arg0 not string")
	}
	var p32 int32
	if v, ok := msg.Arguments[1].(int32); ok {
		p32 = v
	} else if v, ok := msg.Arguments[1].(int64); ok {
		p32 = int32(v)
	} else {
		return "", 0, "", fmt.Errorf("ParseIDsRequest: arg1 not int32/int64")
	}
	id, ok := msg.Arguments[2].(string)
	if !ok {
		return "", 0, "", fmt.Errorf("ParseIDsRequest: arg2 not string")
	}
	return ip, int(p32), id, nil
}

// BuildIDsResponse: s reqID, i count, dann pro Peer: b uid16, s name, s ip, i port
func BuildIDsResponse(reqID string, peers [][]interface{}) *osc.Message {
	m := osc.NewMessage(AddrIDsResponse)
	m.Append(reqID)
	m.Append(int32(len(peers)))
	for _, p := range peers {
		for _, v := range p {
			m.Append(v)
		}
	}
	return m
}

// ---------- /beacon/tags ----------

// ParseTagsRequest: s reply_ip, i reply_port, s request_id, s target_uid_hex[, i max_age_ms]
func ParseTagsRequest(msg *osc.Message) (replyIP string, replyPort int, reqID string, targetUID string, maxAgeMs int, err error) {
	if len(msg.Arguments) < 4 {
		return "", 0, "", "", 0, fmt.Errorf("ParseTagsRequest: want >=4 args, got %d", len(msg.Arguments))
	}
	ip, ok := msg.Arguments[0].(string)
	if !ok {
		return "", 0, "", "", 0, fmt.Errorf("ParseTagsRequest: arg0 not string")
	}
	var p32 int32
	if v, ok := msg.Arguments[1].(int32); ok {
		p32 = v
	} else if v, ok := msg.Arguments[1].(int64); ok {
		p32 = int32(v)
	} else {
		return "", 0, "", "", 0, fmt.Errorf("ParseTagsRequest: arg1 not int32/int64")
	}
	id, ok := msg.Arguments[2].(string)
	if !ok {
		return "", 0, "", "", 0, fmt.Errorf("ParseTagsRequest: arg2 not string")
	}
	target, ok := msg.Arguments[3].(string)
	if !ok {
		return "", 0, "", "", 0, fmt.Errorf("ParseTagsRequest: arg3 not string")
	}
	age := 0
	if len(msg.Arguments) >= 5 {
		if a32, ok := msg.Arguments[4].(int32); ok {
			age = int(a32)
		} else if a64, ok := msg.Arguments[4].(int64); ok {
			age = int(a64)
		}
	}
	return ip, int(p32), id, target, age, nil
}

// BuildTagsRequest: s reply_ip, i reply_port, s reqID, s target_uid_hex
func BuildTagsRequest(replyIP string, replyPort int, reqID, targetUID string) *osc.Message {
	m := osc.NewMessage(AddrTagsRequest)
	m.Append(replyIP)
	m.Append(int32(replyPort))
	m.Append(reqID)
	m.Append(targetUID)
	return m
}

// BuildTagsResponse: b uid16, s name, s ip, i port, s reqID, i n, n× s tag
func BuildTagsResponse(uid []byte, name, ip string, port int, reqID string, tags []string) *osc.Message {
	m := osc.NewMessage(AddrTagsResponse)
	m.Append(uid)
	m.Append(name)
	m.Append(ip)
	m.Append(int32(port))
	m.Append(reqID)
	m.Append(int32(len(tags)))
	for _, t := range tags {
		m.Append(t)
	}
	return m
}
