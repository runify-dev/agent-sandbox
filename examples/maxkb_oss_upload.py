# coding=utf-8
"""
把沙箱工作区里的文件边下载边上传到 MaxKB 文件存储（POST /admin/api/oss/file）。

客户端不会把整个文件读进内存：沙箱的响应流直接拼进 multipart 请求体发出去，
并且带上准确的 Content-Length（Django 不支持 chunked 请求体）。

用法：
    url = transfer_to_maxkb_oss(
        scope="<user_id>",
        session="<chat_id>",
        path="output.mp4",
        maxkb_url="http://127.0.0.1:8080",
        maxkb_token="<MaxKB API token>",
    )
    # url == "./oss/file/<file_id>"
"""

import mimetypes
import os
import uuid
from urllib.parse import quote

import requests

SANDBOX_URL = os.environ.get("SANDBOX_URL", "http://127.0.0.1:8090")
SANDBOX_API_KEY = os.environ.get("SANDBOX_API_KEY", "")


class _MultipartStream:
    """把 [前缀, 文件流, 后缀] 串成一个可 read() 的请求体，长度已知"""

    def __init__(self, prefix: bytes, body, body_size: int, suffix: bytes):
        self._parts = [prefix, body, suffix]
        self._len = len(prefix) + body_size + len(suffix)

    def __len__(self):
        return self._len

    def read(self, size: int = -1) -> bytes:
        while self._parts:
            part = self._parts[0]
            if isinstance(part, bytes):
                if size < 0 or size >= len(part):
                    self._parts.pop(0)
                    if part:
                        return part
                    continue
                self._parts[0] = part[size:]
                return part[:size]
            chunk = part.read(size if size > 0 else 1 << 20)
            if chunk:
                return chunk
            self._parts.pop(0)
        return b""


def _field(boundary: str, name: str, value: str) -> bytes:
    return (f'--{boundary}\r\nContent-Disposition: form-data; name="{name}"\r\n\r\n{value}\r\n').encode()


def transfer_to_maxkb_oss(
    scope: str,
    session: str | None,
    path: str,
    maxkb_url: str,
    maxkb_token: str,
    source_type: str = "TEMPORARY_120_MINUTE",
    source_id: str | None = None,
) -> str:
    """返回 MaxKB 的文件地址 ./oss/file/<file_id>，文件名取 path 的最后一段。
    session 为 None 时读取 scope 的长期空间，否则读取这个对话的目录"""
    file_name = path.rsplit("/", 1)[-1]
    content_type = mimetypes.guess_type(file_name)[0] or "application/octet-stream"

    with requests.get(
        f"{SANDBOX_URL}/v1/files/{scope}/{quote(path)}",
        params={"session": session} if session else None,
        headers={"X-API-Key": SANDBOX_API_KEY},
        stream=True,
        timeout=(5, 300),
    ) as src:
        src.raise_for_status()
        size = int(src.headers["Content-Length"])
        src.raw.decode_content = True

        boundary = uuid.uuid4().hex
        prefix = _field(boundary, "source_type", source_type)
        if source_id:
            prefix += _field(boundary, "source_id", source_id)
        prefix += (
            f"--{boundary}\r\n"
            f"Content-Disposition: form-data; name=\"file\"; "
            f"filename=\"{file_name.encode('ascii', 'replace').decode().replace('\"', '_')}\"; "
            f"filename*=UTF-8''{quote(file_name)}\r\n"
            f"Content-Type: {content_type}\r\n\r\n"
        ).encode()
        suffix = f"\r\n--{boundary}--\r\n".encode()

        resp = requests.post(
            f"{maxkb_url.rstrip('/')}/admin/api/oss/file",
            headers={
                "Authorization": f"Bearer {maxkb_token}",
                "Content-Type": f"multipart/form-data; boundary={boundary}",
            },
            data=_MultipartStream(prefix, src.raw, size, suffix),
            timeout=(5, 600),
        )
    resp.raise_for_status()
    body = resp.json()
    if body.get("code") != 200:
        raise RuntimeError(f"MaxKB upload failed: {body}")
    return body["data"]
