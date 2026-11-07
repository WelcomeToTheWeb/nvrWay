# Agent Note: recorder-shutdown-deleter-owns-child

Status: implemented

## Problem

TestRunReturnsWhenLifecycleContextIsCancelled flaked (~2%, reproduced in golang:1.26 with ffmpeg: "timed out after 3s waiting for every child to be terminated"). It is not only a test problem: an ffmpeg child that outlives the recorder also outlives the server process at shutdown and keeps writing segments into a directory the process is leaving. The S10-03 stranding fix introduced the race — the supervisor goroutine deletes its r.runs entry when the loop unwinds, and on shutdown that delete can beat stopAll's snapshot of the map, so stopAll never sees the run and never terminates its child.

## Decision

The deleter owns the child: after the identity-checked delete in startRunLocked's goroutine (media/recorder.go:637), the goroutine calls run.stop(). run.stop() is cancel + current-process Stop, and Stop is idempotent on both process types (sync.Once on execProcess, closed-guard on the test fake), so stopAll calling it again on the same run is free. The shutdown contract is now: every run is stopped by whoever last removed it from r.runs — stopAll for runs still in the map at cancel time, the unwinding goroutine for runs that left it first. The other S11-01 flake (api TestCamerasCameraWriteBroadcastsCameraStatus) was the test helper, not the product: watchConsole returned right after the WS dial, but the hub registers the client a beat after the handshake, so a camera write broadcasting in that window reached nobody; the helper now waits for the first presence frame, which the hub broadcasts immediately after h.add and is therefore the proof the socket is in the client set.
## Alternatives considered

Raising the 3s test timeouts lost — it hides both races and the leak stays. A hub-side "registered" ack frame lost — presence already proves registration, and a wire change for a test problem is the decoration this repo has called out twice. Making the race deterministic with a test hook into the deleter goroutine lost — the 2/100 mutation reproduction (comment out only line 637) plus the original container failure are the evidence; contorting the product for a deterministic repro is the worse trade.
## Consequences

Server shutdown now terminates every ffmpeg child deterministically regardless of goroutine scheduling; the media suite runs 100/100. Costs: run.stop() now runs on two paths for the same run (harmless only while Stop stays idempotent — a future process type must keep that guarantee); the api helper adds one presence frame of latency before a test may write (milliseconds, and it turns a 3s-timeout flake into a 5s "hub did not register" failure that names the real cause).

