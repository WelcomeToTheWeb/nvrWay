"""nvrWay's detection worker.

One job: read frames from go2rtc's per-camera mjpeg tap, run YOLOv8 (ONNX, CPU by
default), decide whether a box satisfies a camera's zone rules, and tell the
server about it as an `open` / `extend` / `close` signal under a stable
`event_key`.

Three rules the whole package is built on (plan Steps 5 and 6):

*   **The server's clock is the timeline.** A signal carries the worker's own
    timestamp, and the server validates its shape and then discards it. A
    detection box on a machine whose clock is wrong must not be able to file an
    event in the future, so nothing here is trusted to say *when*.
*   **The worker is stateless about the installation.** It asks the server for its
    camera set (`GET /api/internal/v1/detector/config`) every 5 seconds and
    reconciles its threads to it. Editing a camera in the console changes what the
    worker does within one poll, without a restart and without a message.
*   **A dropped signal is a smaller event, never a lost one.** Signals are buffered
    and re-sent; when the buffer overflows the oldest is dropped and counted in a
    log line, because the server dedupes by `event_key` and an event whose opening
    signal was lost is still an event once its next extend arrives.
"""

__version__ = "0.1.0"
