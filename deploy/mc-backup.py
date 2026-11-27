#!/usr/bin/env python3
"""RCON-safe Minecraft world backup script with automatic retention pruning.

Runs save-off + save-all flush over local RCON, tars the world directory into
/opt/mc-backups, always re-enables save-on, and keeps only the newest KEEP_COUNT
backups.
"""

import glob
import os
import socket
import struct
import subprocess
import time
from datetime import datetime

MC_DIR = os.environ.get("MC_DIR", "/opt/minecraft")
BACKUP_DIR = os.environ.get("MC_BACKUP_DIR", "/opt/mc-backups")
KEEP_COUNT = int(os.environ.get("MC_BACKUP_KEEP", "2"))


def read_properties(path: str) -> dict[str, str]:
    props: dict[str, str] = {}
    with open(path, "r", encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            k, v = line.split("=", 1)
            props[k.strip()] = v.strip()
    return props


class RCON:
    def __init__(self, host: str, port: int, password: str, timeout: int = 15) -> None:
        self.s = socket.create_connection((host, port), timeout=timeout)
        self.s.settimeout(timeout)
        self.req_id = 0
        self._auth(password)

    def _send(self, ptype: int, body: str) -> int:
        self.req_id += 1
        data = body.encode("utf-8") + b"\x00\x00"
        pkt = struct.pack("<iii", len(data) + 8, self.req_id, ptype) + data
        self.s.sendall(pkt)
        return self.req_id

    def _recv(self) -> tuple[int, int, str]:
        hdr = b""
        while len(hdr) < 4:
            chunk = self.s.recv(4 - len(hdr))
            if not chunk:
                raise EOFError("rcon closed")
            hdr += chunk
        size = struct.unpack("<i", hdr)[0]
        payload = b""
        while len(payload) < size:
            chunk = self.s.recv(size - len(payload))
            if not chunk:
                raise EOFError("rcon closed")
            payload += chunk
        rid, rtype = struct.unpack("<ii", payload[:8])
        body = payload[8:-2].decode("utf-8", errors="replace")
        return rid, rtype, body

    def _auth(self, password: str) -> None:
        rid = self._send(3, password)
        for _ in range(2):
            resp_id, resp_type, _ = self._recv()
            if resp_type == 2:
                if resp_id != rid:
                    raise RuntimeError("RCON auth failed")
                return
        raise RuntimeError("RCON auth failed")

    def cmd(self, command: str) -> str:
        self._send(2, command)
        _, _, body = self._recv()
        return body

    def close(self) -> None:
        try:
            self.s.close()
        except Exception:
            pass


def main() -> None:
    if subprocess.run(["systemctl", "is-active", "--quiet", "minecraft"]).returncode != 0:
        print("minecraft service not active; skipping backup")
        return

    props = read_properties(os.path.join(MC_DIR, "server.properties"))
    world_name = props.get("level-name", "world")
    rcon_port = int(props.get("rcon.port", "25575"))
    rcon_password = props.get("rcon.password", "")

    world_path = os.path.join(MC_DIR, world_name)
    if not os.path.isdir(world_path):
        print(f"world directory {world_path} not found; skipping")
        return

    os.makedirs(BACKUP_DIR, exist_ok=True)
    ts = datetime.now().strftime("%Y-%m-%d-%H%M%S")
    tmp_file = os.path.join(BACKUP_DIR, f".{world_name}-{ts}.tar.gz.tmp")
    final_file = os.path.join(BACKUP_DIR, f"{world_name}-{ts}.tar.gz")

    rcon = None
    try:
        if rcon_password:
            rcon = RCON("127.0.0.1", rcon_port, rcon_password)
            rcon.cmd("save-off")
            rcon.cmd("save-all flush")
            time.sleep(2)

        subprocess.run(
            ["tar", "-czf", tmp_file, "-C", MC_DIR, world_name],
            check=True,
        )
        os.replace(tmp_file, final_file)
        print(f"Created backup: {final_file}")
    except Exception:
        if os.path.exists(tmp_file):
            os.remove(tmp_file)
        raise
    finally:
        if rcon is not None:
            try:
                rcon.cmd("save-on")
            finally:
                rcon.close()

    backups = sorted(
        glob.glob(os.path.join(BACKUP_DIR, f"{world_name}-*.tar.gz")),
        key=os.path.getmtime,
        reverse=True,
    )
    for old in backups[KEEP_COUNT:]:
        os.remove(old)
        print(f"Removed old backup: {old}")


if __name__ == "__main__":
    main()
