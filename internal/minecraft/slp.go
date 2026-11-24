package minecraft

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	slpProtocolVersion = -1 // Protocol-agnostic status query supported by Java 1.7+
	maxSLPPacketBytes  = 1 << 20
	maxSLPIOTimeout    = 5 * time.Second
)

var formattingCodeRegex = regexp.MustCompile(`§[0-9a-fk-orA-FK-OR]`)

// ServerStatus represents the result of a Java Edition Server List Ping (SLP).
type ServerStatus struct {
	Version       string
	MOTD          string
	OnlinePlayers int
	MaxPlayers    int
	PlayerNames   []string
	Latency       time.Duration
}

type slpResponsePayload struct {
	Version struct {
		Name     string `json:"name"`
		Protocol int    `json:"protocol"`
	} `json:"version"`
	Players struct {
		Max    int `json:"max"`
		Online int `json:"online"`
		Sample []struct {
			Name string `json:"name"`
			ID   string `json:"id"`
		} `json:"sample"`
	} `json:"players"`
	Description json.RawMessage `json:"description"`
}

// QueryStatus performs a Java Edition Server List Ping over TCP and returns live server telemetry.
func QueryStatus(ctx context.Context, host string, port int) (ServerStatus, error) {
	if host == "" {
		return ServerStatus{}, errors.New("minecraft host is empty")
	}
	if port == 0 {
		port = 25565
	}
	if port < 1 || port > 65535 {
		return ServerStatus{}, fmt.Errorf("invalid minecraft port: %d", port)
	}

	dialer := net.Dialer{Timeout: maxSLPIOTimeout}
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return ServerStatus{}, fmt.Errorf("dial minecraft slp %s: %w", addr, err)
	}
	defer conn.Close()

	// Cap per-probe I/O deadline and abort immediately on early context cancellation.
	deadline := time.Now().Add(maxSLPIOTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)
	stopCancel := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
	})
	defer stopCancel()

	// 1. Handshake packet (Packet ID 0x00, Next State = 1 for Status)
	var handshake bytes.Buffer
	writeVarInt(&handshake, 0x00)
	writeVarInt(&handshake, slpProtocolVersion)
	writeMCString(&handshake, host)
	_ = binary.Write(&handshake, binary.BigEndian, uint16(port))
	writeVarInt(&handshake, 1)

	if err := writePacket(conn, handshake.Bytes()); err != nil {
		return ServerStatus{}, fmt.Errorf("send slp handshake: %w", err)
	}

	// 2. Status Request packet (Packet ID 0x00, empty payload)
	start := time.Now()
	if err := writePacket(conn, []byte{0x00}); err != nil {
		return ServerStatus{}, fmt.Errorf("send slp status request: %w", err)
	}

	// 3. Status Response packet
	reader := bufio.NewReader(conn)
	packetID, payload, err := readPacket(reader)
	if err != nil {
		return ServerStatus{}, fmt.Errorf("read slp status response: %w", err)
	}
	fallbackLatency := time.Since(start)
	if packetID != 0x00 {
		return ServerStatus{}, fmt.Errorf("unexpected slp response packet id: 0x%x", packetID)
	}

	jsonStr, err := readMCString(bytes.NewReader(payload))
	if err != nil {
		return ServerStatus{}, fmt.Errorf("decode slp json string: %w", err)
	}

	var raw slpResponsePayload
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return ServerStatus{}, fmt.Errorf("unmarshal slp status json: %w", err)
	}

	// 4. Optional Ping/Pong packet for accurate round-trip latency
	latency := fallbackLatency
	var ping bytes.Buffer
	writeVarInt(&ping, 0x01)
	nonce := time.Now().UnixMilli()
	_ = binary.Write(&ping, binary.BigEndian, nonce)
	pingStart := time.Now()
	if err := writePacket(conn, ping.Bytes()); err == nil {
		if pongID, pongPayload, err := readPacket(reader); err == nil && pongID == 0x01 && len(pongPayload) == 8 {
			var echoed int64
			if binary.Read(bytes.NewReader(pongPayload), binary.BigEndian, &echoed) == nil && echoed == nonce {
				latency = time.Since(pingStart)
			}
		}
	}

	names := make([]string, 0, len(raw.Players.Sample))
	for _, p := range raw.Players.Sample {
		name := strings.TrimSpace(stripFormatting(p.Name))
		if name != "" {
			names = append(names, name)
		}
	}

	return ServerStatus{
		Version:       strings.TrimSpace(stripFormatting(raw.Version.Name)),
		MOTD:          strings.TrimSpace(stripFormatting(parseDescription(raw.Description))),
		OnlinePlayers: raw.Players.Online,
		MaxPlayers:    raw.Players.Max,
		PlayerNames:   names,
		Latency:       latency,
	}, nil
}

func parseDescription(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return plain
	}
	var obj any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	var sb strings.Builder
	extractChatText(&sb, obj)
	return sb.String()
}

func extractChatText(sb *strings.Builder, v any) {
	switch val := v.(type) {
	case string:
		sb.WriteString(val)
	case []any:
		for _, item := range val {
			extractChatText(sb, item)
		}
	case map[string]any:
		if text, ok := val["text"].(string); ok {
			sb.WriteString(text)
		}
		if extra, ok := val["extra"]; ok {
			extractChatText(sb, extra)
		}
	}
}

func stripFormatting(s string) string {
	return formattingCodeRegex.ReplaceAllString(s, "")
}

func writePacket(w io.Writer, body []byte) error {
	var frame bytes.Buffer
	writeVarInt(&frame, int32(len(body)))
	frame.Write(body)
	_, err := w.Write(frame.Bytes())
	return err
}

func readPacket(r *bufio.Reader) (int32, []byte, error) {
	length, err := readVarInt(r)
	if err != nil {
		return 0, nil, err
	}
	if length <= 0 || length > maxSLPPacketBytes {
		return 0, nil, fmt.Errorf("invalid slp packet length: %d", length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	br := bytes.NewReader(buf)
	packetID, err := readVarInt(br)
	if err != nil {
		return 0, nil, err
	}
	rest, err := io.ReadAll(br)
	if err != nil {
		return 0, nil, err
	}
	return packetID, rest, nil
}

func writeVarInt(buf *bytes.Buffer, value int32) {
	u := uint32(value)
	for {
		b := byte(u & 0x7F)
		u >>= 7
		if u != 0 {
			b |= 0x80
		}
		buf.WriteByte(b)
		if u == 0 {
			break
		}
	}
}

func readVarInt(r io.ByteReader) (int32, error) {
	var result uint32
	for shift := uint(0); shift < 35; shift += 7 {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		result |= uint32(b&0x7F) << shift
		if (b & 0x80) == 0 {
			return int32(result), nil
		}
	}
	return 0, errors.New("varint too big")
}

func writeMCString(buf *bytes.Buffer, s string) {
	writeVarInt(buf, int32(len(s)))
	buf.WriteString(s)
}

func readMCString(r *bytes.Reader) (string, error) {
	length, err := readVarInt(r)
	if err != nil {
		return "", err
	}
	if length < 0 || length > maxSLPPacketBytes {
		return "", fmt.Errorf("invalid mc string length: %d", length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}
