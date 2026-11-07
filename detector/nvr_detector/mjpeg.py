"""The frame source: go2rtc's per-camera mjpeg tap, decoded.

The worker never opens a camera itself. go2rtc owns the RTSP source and
transcodes it to a small mjpeg stream (`<slug>_det`) sized for inference, and this
module reads that stream:

    GET http://go2rtc:1984/api/stream.mjpeg?src=<slug>_det

which answers `multipart/x-mixed-replace`, one JPEG per part.

Two rules worth stating, because they are why this module is a class and not one
line of `cv2.VideoCapture`:

*   **The boundary is read from the response, never assumed.** go2rtc's boundary
    is its own choice and it can change between builds; a hard-coded
    `--boundary` is a parser that breaks on a Tuesday.
*   **A socket that closes is the ordinary case, not the error case.** go2rtc
    restarts when a camera config is written (S3-07), a camera renegotiates, an
    idle source is dropped. The iterator reconnects after 5 s and keeps yielding,
    so a camera blip costs frames and not the pipeline.

`cv2.VideoCapture` with an HTTP URL is deliberately not used: it buffers, it
swallows the boundary handling into a backend that cannot be inspected, and a
hung capture there is a hung thread here with no way to say why.
"""

from __future__ import annotations

import logging
import re
import time
from typing import Callable, Iterator, Optional

import cv2
import numpy as np
import requests

log = logging.getLogger("nvr_detector.mjpeg")

# The plan's reconnect delay. Five seconds is go2rtc's own restart window: shorter
# and the worker hammers a sidecar that is still coming up, longer and a camera
# that came back is still dark on the console.
DEFAULT_BACKOFF_SECONDS = 5.0

# One read of the socket. Parts are tens of kilobytes; anything much larger just
# copies bytes we have to scan anyway.
_CHUNK = 65536

_JPEG_SOI = b"\xff\xd8"

# `boundary=` with or without quotes, and go2rtc sometimes sends the delimiter
# itself with its leading dashes stripped and sometimes with them.
_BOUNDARY_RE = re.compile(r"boundary\s*=\s*(?P<b>\"[^\"]*\"|'[^']*'|[^;\s]+)", re.IGNORECASE)


def parse_boundary(content_type: Optional[str]) -> Optional[str]:
    """The multipart boundary named by a Content-Type header, without leading dashes."""
    if not content_type:
        return None
    match = _BOUNDARY_RE.search(content_type)
    if not match:
        return None
    boundary = match.group("b").strip()
    if len(boundary) >= 2 and boundary[0] == boundary[-1] and boundary[0] in "\"'":
        boundary = boundary[1:-1]
    while boundary.startswith("--"):
        boundary = boundary[2:]
    return boundary or None


def stream_url(base_url: str, stream: str) -> str:
    """The mjpeg endpoint for one go2rtc stream name.

    `base_url` is the operator's `GO2RTC_URL` (default `http://go2rtc:1984`, the
    same env row the server reads), and `stream` is `<slug>_det` as the detector
    config payload names it. The slug is URL-safe by construction — the camera
    slug contract is `^[a-z0-9][a-z0-9-]{1,31}$` — so there is nothing to escape.
    """
    return f"{base_url.rstrip('/')}/api/stream.mjpeg?src={stream}"


def decode_part(part: bytes) -> Optional[np.ndarray]:
    """One multipart part to a BGR frame, or None when it is not a decodable JPEG.

    A part is `headers CRLF CRLF body CRLF`. Some servers prepend a CRLF before the
    first header, and every part ends with one, so the body is trimmed at both
    ends; a JPEG's first two bytes are its SOI marker, which is a cheap way to
    skip a preamble without trying to decode it.
    """
    _, sep, body = part.partition(b"\r\n\r\n")
    if not sep:
        body = part
    body = body.strip(b"\r\n")
    if not body.startswith(_JPEG_SOI):
        return None
    frame = cv2.imdecode(np.frombuffer(body, dtype=np.uint8), cv2.IMREAD_COLOR)
    if frame is None or frame.size == 0:
        return None
    return frame


def _default_opener(
    url: str, timeout: float, auth: Optional[tuple[str, str]] = None
) -> requests.Response:
    return requests.get(
        url,
        stream=True,
        timeout=timeout,
        auth=auth,
        headers={"Accept": "multipart/x-mixed-replace"},
    )


class MjpegStream:
    """An iterator over decoded BGR frames from one mjpeg endpoint.

    `opener` is the seam the suite uses: it takes `(url, timeout)` and returns
    anything with `.headers` and `.iter_content()`, so the boundary, the reconnect
    and the timeout are all testable without a network. `sleep` is the same idea
    for the 5 s backoff.
    """

    def __init__(
        self,
        url: str,
        *,
        name: str = "",
        timeout: float = 10.0,
        backoff: float = DEFAULT_BACKOFF_SECONDS,
        opener: Optional[Callable[[str, float], object]] = None,
        auth: Optional[tuple[str, str]] = None,
        sleep: Callable[[float], None] = time.sleep,
        stop: Optional["object"] = None,
    ) -> None:
        self.url = url
        self.name = name or url
        self.timeout = float(timeout)
        self.backoff = float(backoff)
        # go2rtc's API is behind basic auth the moment `api.username` is set
        # (S9-04), and the tap is part of that API: without credentials the
        # endpoint answers 401 as `text/plain`, which reads as "not an mjpeg
        # stream" forever. `auth` is (user, password) or None for an open API.
        self.auth = auth
        self._opener = opener or (lambda u, t: _default_opener(u, t, auth))
        self._sleep = sleep
        self._stop = stop

        self.closed = False
        self.boundary: Optional[str] = None
        self.frames = 0
        self.reconnects = 0
        self.undecodable = 0

    def close(self) -> None:
        """Stop iterating. A thread blocked in `iter_content` still has to be
        released by closing the response, which the owning thread does when its
        loop ends; this says "do not reconnect"."""
        self.closed = True

    def _should_stop(self) -> bool:
        if self._stop is not None and getattr(self._stop, "is_set", lambda: False)():
            return True
        return self.closed

    def _wait_for_reconnect(self) -> bool:
        """Sleep out the backoff. True means the stream should stop."""
        if self._should_stop():
            return True
        self._sleep(self.backoff)
        return self._should_stop()

    def __iter__(self) -> Iterator[np.ndarray]:
        while not self._should_stop():
            response = None
            try:
                response = self._opener(self.url, self.timeout)
                content_type = _header(response, "Content-Type")
                boundary = parse_boundary(content_type)
                if boundary is None:
                    raise ValueError(
                        "no multipart boundary in Content-Type "
                        f"{content_type!r}; the endpoint answered something that is "
                        "not an mjpeg stream"
                    )
                self.boundary = boundary
                yield from self._parts(response, boundary)
            except Exception as exc:  # noqa: BLE001 - a dead socket is the reason, whatever its class
                log.warning("mjpeg: %s: %s", self.name, exc)
            finally:
                _close(response)
            if self._wait_for_reconnect():
                return
            self.reconnects += 1
            log.info(
                "mjpeg: %s: reconnecting in %.0fs (after %d reconnects)",
                self.name,
                self.backoff,
                self.reconnects,
            )

    def _parts(self, response: object, boundary: str) -> Iterator[np.ndarray]:
        """Split the response on its boundary and decode each completed part.

        A part is only complete when the *next* delimiter has arrived, so the
        buffer keeps whatever follows the last delimiter seen — that is the part
        being written now, and reading it early is how a half JPEG gets decoded
        into nothing.
        """
        delimiter = b"--" + boundary.encode("utf-8", "ignore")
        buf = bytearray()
        started = False
        for chunk in response.iter_content(chunk_size=_CHUNK):  # type: ignore[attr-defined]
            if self._should_stop():
                return
            if not chunk:
                continue
            buf += chunk
            while True:
                index = buf.find(delimiter)
                if index < 0:
                    break
                if not started:
                    # Everything before the first delimiter is a preamble.
                    del buf[: index + len(delimiter)]
                    started = True
                    continue
                part = bytes(buf[:index])
                del buf[: index + len(delimiter)]
                frame = decode_part(part)
                if frame is None:
                    self.undecodable += 1
                    continue
                self.frames += 1
                yield frame


def _header(response: object, name: str) -> Optional[str]:
    headers = getattr(response, "headers", None)
    if headers is None:
        return None
    try:
        return headers.get(name)
    except AttributeError:
        return None


def _close(response: object) -> None:
    close = getattr(response, "close", None)
    if callable(close):
        try:
            close()
        except Exception:  # noqa: BLE001, S110 - closing a dead socket can fail; there is nothing to do about it
            pass
