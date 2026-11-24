package minecraft

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestQueryStatus_PlainAndStructuredMOTD(t *testing.T) {
	cases := []struct {
		name     string
		response string
		wantMOTD string
	}{
		{
			name: "plain string description with formatting codes",
			response: `{
				"version": {"name": "Paper 1.21.4", "protocol": 769},
				"players": {"max": 20, "online": 2, "sample": [{"name": "Steve", "id": "1"}, {"name": "Alex", "id": "2"}]},
				"description": "§aWelcome to §bCloudCraft!"
			}`,
			wantMOTD: "Welcome to CloudCraft!",
		},
		{
			name: "chat component description",
			response: `{
				"version": {"name": "1.21.4", "protocol": 769},
				"players": {"max": 10, "online": 1, "sample": [{"name": "Herobrine", "id": "3"}]},
				"description": {"text": "Survival ", "extra": [{"text": "SMP", "color": "gold"}]}
			}`,
			wantMOTD: "Survival SMP",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			defer ln.Close()

			go func() {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				r := bufio.NewReader(conn)
				// Read handshake
				if _, _, err := readPacket(r); err != nil {
					return
				}
				// Read status request
				if _, _, err := readPacket(r); err != nil {
					return
				}
				// Write status response
				var resp bytes.Buffer
				writeVarInt(&resp, 0x00)
				writeMCString(&resp, tc.response)
				_ = writePacket(conn, resp.Bytes())

				// Echo ping packet
				if id, payload, err := readPacket(r); err == nil && id == 0x01 {
					var pong bytes.Buffer
					writeVarInt(&pong, 0x01)
					pong.Write(payload)
					_ = writePacket(conn, pong.Bytes())
				}
			}()

			tcpAddr := ln.Addr().(*net.TCPAddr)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			st, err := QueryStatus(ctx, "127.0.0.1", tcpAddr.Port)
			if err != nil {
				t.Fatalf("QueryStatus error: %v", err)
			}
			if st.MOTD != tc.wantMOTD {
				t.Errorf("MOTD = %q, want %q", st.MOTD, tc.wantMOTD)
			}
			if st.OnlinePlayers == 0 || len(st.PlayerNames) != st.OnlinePlayers {
				t.Errorf("unexpected players: online=%d names=%v", st.OnlinePlayers, st.PlayerNames)
			}
		})
	}
}

func TestRCON_AuthAndExec(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	const secret = "s3cr3t"
	executed := make(chan string, 4)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				srv := &RCONClient{conn: c}
				id, pType, body, err := srv.readPacket()
				if err != nil || pType != rconPacketAuth {
					return
				}
				if body != secret {
					_ = srv.writePacket(-1, rconPacketAuthResponse, "")
					return
				}
				// Mimic real Minecraft RCON sending empty response before auth response
				_ = srv.writePacket(id, rconPacketResponse, "")
				_ = srv.writePacket(id, rconPacketAuthResponse, "")

				for {
					cmdID, cmdType, cmdBody, err := srv.readPacket()
					if err != nil {
						if err == io.EOF {
							return
						}
						return
					}
					if cmdType == rconPacketExecCommand {
						executed <- cmdBody
						_ = srv.writePacket(cmdID, rconPacketResponse, "ok:"+cmdBody)
					}
				}
			}(conn)
		}
	}()

	tcpAddr := ln.Addr().(*net.TCPAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Bad password should fail with ErrRCONAuthFailed
	if _, err := DialRCON(ctx, "127.0.0.1", tcpAddr.Port, "wrong"); err != ErrRCONAuthFailed {
		t.Fatalf("expected ErrRCONAuthFailed, got %v", err)
	}

	// Good password should authenticate and execute commands
	client := &Client{RCONPort: tcpAddr.Port, RCONPassword: secret}
	if err := client.SendDiscordChat(ctx, "127.0.0.1", "Alice", "hello \"world\"\n/op evil"); err != nil {
		t.Fatalf("SendDiscordChat error: %v", err)
	}

	got := <-executed
	if !strings.HasPrefix(got, "tellraw @a ") {
		t.Fatalf("expected tellraw prefix, got %q", got)
	}
	var parts []tellrawPart
	if err := json.Unmarshal([]byte(strings.TrimPrefix(got, "tellraw @a ")), &parts); err != nil {
		t.Fatalf("tellraw JSON invalid: %v (%s)", err, got)
	}
	if len(parts) != 3 || parts[1].Text != "Alice" || strings.Contains(parts[2].Text, "\n") {
		t.Fatalf("unexpected tellraw parts: %+v", parts)
	}
}
