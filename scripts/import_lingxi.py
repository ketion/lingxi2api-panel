"""导入 WPS 灵犀账号到 lingxi2api。

两种来源：
  1) 直接给 wps_sid（最快）
       python scripts/import_lingxi.py --sid "V02S-xxxx"
  2) 从本机灵犀客户端自动读取（需先退出客户端 —— 它运行时会独占 Cookie 库）
       python scripts/import_lingxi.py

写入 auths/<uid>.json：{"wps_sid": "...", "uid": "...", "nickname": "..."}
网关启动时会自动加载该目录下所有 .json。
"""
import argparse
import base64
import ctypes
import json
import os
import sqlite3
import sys
import tempfile
from pathlib import Path

try:
    from cryptography.hazmat.primitives.ciphers.aead import AESGCM
    HAS_CRYPTO = True
except ImportError:
    HAS_CRYPTO = False

CLIENT_DIRS = [
    Path(os.environ.get("APPDATA", "")) / "WPS 灵犀",
    Path(os.environ.get("APPDATA", "")) / "lingxi-desktop",
]
AUTH_DIR = Path(__file__).resolve().parent.parent / "auths"


class DATA_BLOB(ctypes.Structure):
    _fields_ = [("cbData", ctypes.c_ulong), ("pbData", ctypes.POINTER(ctypes.c_char))]


def dpapi_unprotect(data: bytes) -> bytes:
    crypt32 = ctypes.WinDLL("crypt32", use_last_error=True)
    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    buf = ctypes.create_string_buffer(data, len(data))
    blob_in = DATA_BLOB(len(data), ctypes.cast(buf, ctypes.POINTER(ctypes.c_char)))
    blob_out = DATA_BLOB()
    if not crypt32.CryptUnprotectData(
        ctypes.byref(blob_in), None, None, None, None, 0, ctypes.byref(blob_out)
    ):
        raise OSError(ctypes.get_last_error(), "CryptUnprotectData 失败")
    try:
        return ctypes.string_at(blob_out.pbData, blob_out.cbData)
    finally:
        kernel32.LocalFree(blob_out.pbData)


def read_from_client() -> str:
    if not HAS_CRYPTO:
        raise RuntimeError("缺少 cryptography 库：pip install cryptography")
    profile = next((d for d in CLIENT_DIRS if (d / "Local State").exists()), None)
    if not profile:
        raise RuntimeError("没找到灵犀客户端数据目录")
    state = json.loads((profile / "Local State").read_text(encoding="utf-8"))
    key = dpapi_unprotect(base64.b64decode(state["os_crypt"]["encrypted_key"])[5:])

    from urllib.parse import quote

    def query(path: Path):
        con = sqlite3.connect("file:" + quote(path.as_posix()) + "?mode=ro&immutable=1", uri=True)
        try:
            return con.execute(
                "SELECT encrypted_value, value FROM cookies WHERE name='wps_sid'"
            ).fetchall()
        finally:
            con.close()

    candidates = [profile / "Network" / "Cookies"]
    parts = profile / "Partitions"
    if parts.exists():
        candidates += [p / "Network" / "Cookies" for p in parts.iterdir() if (p / "Network" / "Cookies").exists()]

    for db in candidates:
        if not db.exists():
            continue
        try:
            rows = query(db)
        except Exception:
            continue
        for enc, plain in rows:
            if plain:
                return plain
            if not enc:
                continue
            try:
                if enc[:3] == b"v10":
                    val = AESGCM(key).decrypt(enc[3:15], enc[15:], None).decode()
                elif enc[:3] == b"v20":
                    continue  # app-bound，需要客户端进程内密钥
                else:
                    val = dpapi_unprotect(enc).decode()
                if val:
                    return val
            except Exception:
                continue
    raise RuntimeError("没读到 wps_sid（客户端可能在运行并锁库，先退出客户端再试）")


def write_auth(sid: str, nickname: str) -> Path:
    sid = sid.strip()
    if not sid:
        raise ValueError("wps_sid 为空")
    uid = "sid-" + sid[-8:]
    AUTH_DIR.mkdir(parents=True, exist_ok=True)
    path = AUTH_DIR / f"{uid}.json"
    path.write_text(
        json.dumps({"wps_sid": sid, "uid": uid, "nickname": nickname}, ensure_ascii=False, indent=2),
        encoding="utf-8",
    )
    return path


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--sid", help="直接指定 wps_sid")
    ap.add_argument("--name", default="灵犀账号", help="备注名")
    args = ap.parse_args()

    sid = args.sid or read_from_client()
    path = write_auth(sid, args.name)
    print(f"已写入 {path}")
    print(f"wps_sid 尾部: ...{sid[-8:]}")
    print("重启网关或等面板刷新即可看到该账号")


if __name__ == "__main__":
    sys.exit(main())
